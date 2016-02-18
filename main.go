package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
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

type server struct {
	upstream  string
	cacheRoot string

	upstreamLocksMu sync.Mutex
	upstreamLocks   map[string]sync.Mutex
}

func (s server) localDir(upstream string) string {
	// TODO escape special characters
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

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log.Printf("%s %s %s %v", req.Method, req.URL, req.Proto, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		repoPath := strings.TrimSuffix(req.URL.Path, "/info/refs")
		if !strings.HasSuffix(repoPath, ".git") {
			repoPath = repoPath + ".git"
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		fmt.Fprint(w, "001e# service=git-upload-pack\n")
		fmt.Fprint(w, "0000")

		upstream := s.upstream + repoPath
		dir := s.localDir(upstream)
		cmd := exec.Command("git", "upload-pack", "--advertise-refs", dir)
		cmd.Stdout = w
		cmd.Stderr = os.Stderr
		err := cmd.Run()
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

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[%s] %q", upstream, line)
			line = strings.Replace(line, " no-done", "        ", -1)
			fmt.Fprintln(w, line)
		}
		if err := scanner.Err(); err != nil {
			httpFatal(w, err)
			return
		}

		// TODO: do "git pull" or "git clone --mirror" with lock only if the refs do not match
		// if err := s.updateLocalCache(upstream); err != nil {
		// 	httpFatal(w, err)
		// 	return
		// }
	} else if false && req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		resp, err := http.Post("https://github.com"+req.URL.Path, req.Header.Get("Content-Type"), req.Body)
		if err != nil {
			log.Fatal(err)
		}

		for k, v := range resp.Header {
			log.Printf("%v %v", k, v)
			for _, v := range v {
				w.Header().Set(k, v)
			}
		}
		r := io.TeeReader(resp.Body, os.Stderr)
		io.Copy(w, r)
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
		// cmd.Stdout = bufio.NewWriterSize(w, 8)
		// cmd.Stdin = io.TeeReader(r, os.Stderr)
		cmd.Stdin = r
		cmd.Stderr = os.Stderr // TODO: to log
		// b, err := cmd.Output()
		// if err != nil {
		// 	log.Println(err)
		// 	return
		// }
		// w.Write(b)
		// ioutil.WriteFile("tmp", b, 0777)

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
	go func() {
		u, _ := url.Parse("https://github.com")
		p := httputil.NewSingleHostReverseProxy(u)
		err := http.ListenAndServe("localhost:9281", p)
		if err != nil {
			log.Println(err)
		}
	}()

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
	_ = s
}
