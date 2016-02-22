package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"compress/gzip"
	"net/http"
	"net/url"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
}

type upstreamRepo struct {
	URL *url.URL
}

type server struct {
	upstream string
	basePath string

	repoMutexes struct {
		sync.Mutex
		m map[string]sync.RWMutex
	}
}

func (s server) upstreamRepo(path string) (*upstreamRepo, error) {
	if !strings.HasSuffix(path, ".git") {
		path = path + ".git"
	}
	u, err := url.Parse(s.upstream + path)
	if err != nil {
		return nil, err
	}

	return &upstreamRepo{URL: u}, nil
}

func (s server) localDir(repo *upstreamRepo) string {
	path := append([]string{s.basePath, repo.URL.Host}, strings.Split(repo.URL.Path, "/")...)
	return filepath.Join(path...)
}

func (s *server) repoMutex(repo *upstreamRepo) *sync.RWMutex {
	s.repoMutexes.Lock()
	defer s.repoMutexes.Unlock()

	if s.repoMutexes.m == nil {
		s.repoMutexes.m = map[string]sync.RWMutex{}
	}

	repoURL := repo.URL.String()
	mu, ok := s.repoMutexes.m[repoURL]
	if !ok {
		mu = sync.RWMutex{}
		s.repoMutexes.m[repoURL] = mu
	}

	return &mu
}

func runCommandLogged(cmd *exec.Cmd) error {
	var wg sync.WaitGroup

	outs := []struct {
		name string
		w    io.Writer
		pipe func() (io.ReadCloser, error)
	}{
		{"out", cmd.Stdout, cmd.StdoutPipe},
		{"err", cmd.Stderr, cmd.StderrPipe},
	}

	log.Printf("[command %p] Running %q", cmd, cmd.Args)

	for _, o := range outs {
		if o.w != nil {
			continue
		}

		r, err := o.pipe()
		if err != nil {
			return err
		}

		wg.Add(1)
		go func(name string, r io.Reader) {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				log.Printf("[command %p :: %s] %s", cmd, name, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				log.Printf("[command %p !! %s] error=%v", cmd, name, err)
			}
			wg.Done()
		}(o.name, r)
	}

	start := time.Now()
	cmd.Start()

	wg.Wait()

	err := cmd.Wait()
	log.Printf("[command %p] %q done in %s", cmd, cmd.Args, time.Now().Sub(start))

	return err
}

func (s *server) synchronizeCache(repo *upstreamRepo) error {
	mu := s.repoMutex(repo)
	mu.Lock()
	defer mu.Unlock()

	dir := s.localDir(repo)

	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// cache does not exist, so initialize one (may take long)
			if err := os.MkdirAll(dir, 0777); err != nil {
				return err
			}

			cmd := exec.Command("git", "clone", "--verbose", "--mirror", repo.URL.String(), ".")
			cmd.Dir = dir
			return runCommandLogged(cmd)
		}

		return err
	} else if fi != nil && fi.IsDir() {
		// cache exists, update it
		cmd := exec.Command("git", "remote", "--verbose", "update")
		cmd.Dir = dir
		return runCommandLogged(cmd)
	}

	return fmt.Errorf("cache could not synchronize: %v", repo)
}

func (s *server) advertiseRefs(repo *upstreamRepo, w http.ResponseWriter) {
	// NOTE: serve remote content and defer synchronization to upload-pack phase?
	if err := s.synchronizeCache(repo); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	fmt.Fprint(w, "001e# service=git-upload-pack\n")
	fmt.Fprint(w, "0000")

	mu := s.repoMutex(repo)
	mu.RLock()
	defer mu.RUnlock()

	cmd := exec.Command("git", "upload-pack", "--advertise-refs", ".")
	cmd.Stdout = w
	cmd.Dir = s.localDir(repo)
	err := runCommandLogged(cmd)
	if err != nil {
		log.Println(err)
	}
}

func (s *server) uploadPack(repo *upstreamRepo, w http.ResponseWriter, r io.ReadCloser) {
	mu := s.repoMutex(repo)
	mu.RLock()
	defer mu.RUnlock()

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	defer r.Close()

	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", ".")
	cmd.Stdout = w
	cmd.Stdin = r
	cmd.Dir = s.localDir(repo)
	if err := runCommandLogged(cmd); err != nil {
		log.Println(err)
		return
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log.Printf("[request %p] %s %s %v", req, req.Method, req.URL, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// mode: ref delivery
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/info/refs")
		repo, err := s.upstreamRepo(repoPath)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		s.advertiseRefs(repo, w)
	} else if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		// mode: upload-pack
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/git-upload-pack")
		repo, err := s.upstreamRepo(repoPath)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		r := req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			var err error
			r, err = gzip.NewReader(req.Body)
			if err != nil {
				log.Println(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		s.uploadPack(repo, w, r)
	} else {
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
	}
}

func main() {
	var (
		s      server
		listen string
	)
	flag.StringVar(&s.upstream, "upstream", "", "upstream repositories' base `URL`")
	flag.StringVar(&s.basePath, "base-path", "", "base `directory` for locally cloned repositories")
	flag.StringVar(&listen, "listen", ":9280", "`address` to listen to")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -listen=<addr> -upstream=<url> -base-path=<path>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if s.upstream == "" || s.basePath == "" {
		flag.Usage()
		os.Exit(2)
	}

	log.Printf("[server %p] mir starting at %s ...", &s, listen)

	err := http.ListenAndServe(listen, &s)
	if err != nil {
		log.Println(err)
	}
}
