package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)
}

type server struct {
	upstream  string
	cacheRoot string

	upstreamLocksMu sync.Mutex
	upstreamLocks   map[string]sync.Mutex
}

func (s server) localDir(upstream string) string {
	upstream = strings.Replace(upstream, "/", "-", -1)
	upstream = strings.Replace(upstream, ":", "-", -1)
	return filepath.Join(s.cacheRoot, upstream)
}

func (s server) updateLocalCache(upstream string) error {
	dir := s.localDir(upstream)
	fi, err := os.Stat(dir)
	if fi == nil {
		if os.IsNotExist(err) {
			cmd := exec.Command("git", "clone", "--mirror", upstream, dir)
			cmd.Stdout = os.Stdout // TODO: to log
			cmd.Stderr = os.Stderr
			return cmd.Run()
		} else {
			return err
		}
	} else if fi.IsDir() {
		cmd := exec.Command("git", "remote", "update")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = dir
		return cmd.Run()
	} else {
		return fmt.Errorf("not a directory: %s", dir)
	}
}

func (s *server) getRepos(path string) (string, string) {
	if !strings.HasSuffix(path, ".git") {
		path = path + ".git"
	}
	u := s.upstreamRepo(path)
	return u, s.localDir(u)
}

func (s *server) upstreamRepo(path string) string {
	if !strings.HasSuffix(path, ".git") {
		path = path + ".git"
	}
	return s.upstream + path
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log.Printf("%s %s %s %v", req.Method, req.URL, req.Proto, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		repoPath := strings.TrimSuffix(req.URL.Path, "/info/refs")
		upstreamURL, localDir := s.getRepos(repoPath)

		err := s.updateLocalCache(upstreamURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		fmt.Fprint(w, "001e# service=git-upload-pack\n")
		fmt.Fprint(w, "0000")

		cmd := exec.Command("git", "upload-pack", "--advertise-refs", localDir)
		cmd.Stdout = w
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			log.Println(err)
		}
	} else if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// mode: ref delivery
		repoPath := strings.TrimSuffix(req.URL.Path, "/info/refs")
		if !strings.HasSuffix(repoPath, ".git") {
			repoPath = repoPath + ".git"
		}
		upstream := s.upstream + repoPath

		s.upstreamLocksMu.Lock()

		mu, ok := s.upstreamLocks[upstream]
		if !ok {
			mu = sync.Mutex{}
			s.upstreamLocks[upstream] = mu
		}

		s.upstreamLocksMu.Unlock()

		mu.Lock()
		defer mu.Unlock()

		log.Printf("upstream: %v", upstream)

		resp, err := http.Get(upstream + "/info/refs?service=git-upload-pack")
		if err != nil {
			httpFatal(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")

		io.Copy(w, resp.Body)

		// TODO: do "git pull" or "git clone --mirror" with lock only if the refs do not match
		// if err := s.updateLocalCache(upstream); err != nil {
		// 	httpFatal(w, err)
		// 	return
		// }
	} else if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		// mode: upload-pack
		// TODO: lock
		repoPath := strings.TrimSuffix(req.URL.Path, "/git-upload-pack")
		if !strings.HasSuffix(repoPath, ".git") {
			repoPath = repoPath + ".git"
		}

		var r io.Reader
		if req.Header.Get("Content-Encoding") == "gzip" {
			var err error
			r, err = gzip.NewReader(req.Body)
			if err != nil {
				log.Println(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			r = req.Body
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		// w.Header().Set("Transfer-Encoding", "identity")

		upstream := s.upstream + repoPath
		dir := s.localDir(upstream)
		cmd := exec.Command("git", "upload-pack", "--stateless-rpc", ".")
		cmd.Dir = dir
		cmd.Stdout = w
		cmd.Stdin = r
		cmd.Stderr = os.Stderr // TODO: to log

		if err := cmd.Run(); err != nil {
			log.Println(err)
			// http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		return
	}

	// http.Error(w, "Not Implemented", http.StatusNotImplemented)
}

func httpFatal(w http.ResponseWriter, err error) {
	log.Println(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func main() {
	s := server{
		upstream:      "https://github.com",
		cacheRoot:     "./cache",
		upstreamLocks: map[string]sync.Mutex{},
	}
	log.Println("git-slave-proxy-server starting at :9280 ...")
	err := http.ListenAndServe("localhost:9280", &s)
	if err != nil {
		log.Println(err)
	}
}
