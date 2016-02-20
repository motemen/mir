package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

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

			cmd := exec.Command("git", "clone", "--mirror", repo.URL.String(), ".")
			cmd.Stdout = os.Stdout // TODO: to log
			cmd.Stderr = os.Stderr
			cmd.Dir = dir
			return cmd.Run()
		}

		return err
	} else if fi != nil && fi.IsDir() {
		// cache exists, update it
		cmd := exec.Command("git", "remote", "update")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = dir
		return cmd.Run()
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
	cmd.Stderr = os.Stderr
	cmd.Dir = s.localDir(repo)
	err := cmd.Run()
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
	cmd.Stderr = os.Stderr // TODO: to log
	cmd.Dir = s.localDir(repo)
	if err := cmd.Run(); err != nil {
		log.Println(err)
		return
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log.Printf("%s %s %s %v", req.Method, req.URL, req.Proto, req.Header)

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

	log.Printf("mir starting at %s ...", listen)
	err := http.ListenAndServe(listen, &s)
	if err != nil {
		log.Println(err)
	}
}
