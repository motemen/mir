package main

import (
	"compress/gzip"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
}

type upstreamRepo struct {
	URL *url.URL
}

type server struct {
	upstream  string
	cacheRoot string

	upstreams struct {
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

func (s server) cacheDir(repo *upstreamRepo) string {
	path := append([]string{s.cacheRoot, repo.URL.Host}, strings.Split(repo.URL.Path, "/")...)
	return filepath.Join(path...)
}

func (s *server) repoMutex(repo *upstreamRepo) *sync.RWMutex {
	s.upstreams.Lock()
	defer s.upstreams.Unlock()

	if s.upstreams.m == nil {
		s.upstreams.m = map[string]sync.RWMutex{}
	}

	mu, ok := s.upstreams.m[repo.URL.String()]
	if !ok {
		mu = sync.RWMutex{}
		s.upstreams.m[repo.URL.String()] = mu
	}

	return &mu
}

func (s *server) synchronizeCache(repo *upstreamRepo) error {
	mu := s.repoMutex(repo)
	mu.Lock()
	defer mu.Unlock()

	dir := s.cacheDir(repo)

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
		cmd.Dir = s.cacheDir(repo)
		err = cmd.Run()
		if err != nil {
			log.Println(err)
		}
	} else if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		// mode: upload-pack
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/git-upload-pack")
		repo, err := s.upstreamRepo(repoPath)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		mu := s.repoMutex(repo)
		mu.RLock()
		defer mu.RUnlock()

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

		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")

		cmd := exec.Command("git", "upload-pack", "--stateless-rpc", ".")
		cmd.Stdout = w
		cmd.Stdin = r
		cmd.Stderr = os.Stderr // TODO: to log
		cmd.Dir = s.cacheDir(repo)
		if err := cmd.Run(); err != nil {
			log.Println(err)
			return
		}
	} else {
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
	}
}

func main() {
	s := server{
		upstream:  "https://github.com/",
		cacheRoot: "./cache",
	}
	log.Println("git-slave-proxy-server starting at :9280 ...")
	err := http.ListenAndServe("localhost:9280", &s)
	if err != nil {
		log.Println(err)
	}
}
