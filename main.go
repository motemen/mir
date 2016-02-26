package main

import (
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
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

	"github.com/motemen/go-nuts/logwriter"
)

var logger = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)

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

	packCache struct {
		sync.RWMutex
		m map[string][]byte
	}
}

func (s *server) upstreamRepo(path string) (*upstreamRepo, error) {
	if !strings.HasSuffix(path, ".git") {
		path = path + ".git"
	}
	u, err := url.Parse(s.upstream + path)
	if err != nil {
		return nil, err
	}

	return &upstreamRepo{URL: u}, nil
}

func (s *server) localDir(repo *upstreamRepo) string {
	// TODO(motemen): include protocols (e.g. "https") for completeness
	// Note that "ssh://user@example.com/foo/bar" and "user@example.com:foo/bar" differs
	// (git/Documentation/technical/pack-protocol.txt)
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
	logger.Printf("[command %p] %q starting", cmd, cmd.Args)
	defer logger.Printf("[command %p] %q finished", cmd, cmd.Args)

	for _, s := range []struct {
		writer *io.Writer
		name   string
	}{
		{&cmd.Stdout, "out"},
		{&cmd.Stderr, "err"},
	} {
		if *s.writer == nil {
			*s.writer = &logwriter.LogWriter{
				Logger:     logger,
				Format:     "[command %p :: %s] %s",
				FormatArgs: []interface{}{cmd, s.name},
				Calldepth:  9,
			}
		}
	}
	return cmd.Run()
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

	return fmt.Errorf("could not synchronize cache: %v", repo)
}

func (s *server) advertiseRefs(repo *upstreamRepo, w http.ResponseWriter) {
	// TODO(motemen): Consider serving remote response and move
	// synchronizeCache to another goroutine. Note we have to implement each
	// protocol if we do this, as git does not provide ways to obtain raw
	// git-upload-pack response.
	if err := s.synchronizeCache(repo); err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	fmt.Fprint(w, "001e# service=git-upload-pack\n")
	fmt.Fprint(w, "0000")

	mu := s.repoMutex(repo)
	mu.RLock()
	defer mu.RUnlock()

	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", ".")
	cmd.Stdout = w
	cmd.Dir = s.localDir(repo)
	err := runCommandLogged(cmd)
	if err != nil {
		logger.Println(err)
	}
}

func (s *server) uploadPack(repo *upstreamRepo, w http.ResponseWriter, r io.ReadCloser) {
	mu := s.repoMutex(repo)
	mu.RLock()
	defer mu.RUnlock()

	clientRequest, err := ioutil.ReadAll(r)
	r.Close()

	if err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	if packResponse := s.getPackCache(repo, clientRequest); packResponse != nil {
		resp := make([]byte, len(packResponse))
		copy(resp, packResponse)

		message := "[mir] Using cache\n"
		// 0x02 is the progress information band (see git/Documentation/technical/pack-protocol.txt)
		progressLine := []byte(fmt.Sprintf("%04x\002%s", 4+1+len(message), message))

		if p := bytes.Index(packResponse, []byte("0008NAK\n")); p != -1 {
			resp = append(resp[0:p+0x0008], progressLine...)
			resp = append(resp, packResponse[p+0x0008:]...)
		} else if p := bytes.Index(packResponse, []byte("0031ACK ")); p != -1 {
			resp = append(resp[0:p+0x0031], progressLine...)
			resp = append(resp, packResponse[p+0x0031:]...)
		}

		w.Write(resp)
		return
	}

	var respBody bytes.Buffer

	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", ".")
	cmd.Stdout = &respBody
	cmd.Stdin = bytes.NewBuffer(clientRequest)
	cmd.Dir = s.localDir(repo)
	if err := runCommandLogged(cmd); err != nil {
		logger.Println(err)
		return
	}

	s.setPackCache(repo, clientRequest, respBody.Bytes())
	io.Copy(w, &respBody)
}

func (s *server) getPackCache(repo *upstreamRepo, clientRequest []byte) []byte {
	s.packCache.RLock()
	defer s.packCache.RUnlock()

	reqDigest := sha1.Sum(clientRequest)
	key := repo.URL.String() + "\000" + string(reqDigest[:])
	if s.packCache.m == nil {
		s.packCache.m = map[string][]byte{}
	}

	return s.packCache.m[key]
}

var cacheExpration = time.Second * 100

func (s *server) setPackCache(repo *upstreamRepo, clientRequest []byte, packResponse []byte) {
	s.packCache.Lock()
	defer s.packCache.Unlock()

	reqDigest := sha1.Sum(clientRequest)
	key := repo.URL.String() + "\000" + string(reqDigest[:])
	if s.packCache.m == nil {
		s.packCache.m = map[string][]byte{}
	}

	s.packCache.m[key] = packResponse

	time.AfterFunc(cacheExpration, func() {
		logger.Printf("cache expired: %s", key)
		s.packCache.Lock()
		defer s.packCache.Unlock()
		delete(s.packCache.m, key)
	})
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	logger.Printf("[request %p] %s %s %v", req, req.Method, req.URL, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// mode: ref delivery
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/info/refs")
		repo, err := s.upstreamRepo(repoPath)
		if err != nil {
			logger.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		s.advertiseRefs(repo, w)
	} else if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		// mode: upload-pack
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/git-upload-pack")
		repo, err := s.upstreamRepo(repoPath)
		if err != nil {
			logger.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		r := req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			var err error
			r, err = gzip.NewReader(req.Body)
			if err != nil {
				logger.Println(err)
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

	logger.Printf("[server %p] mir starting at %s ...", &s, listen)

	err := http.ListenAndServe(listen, &s)
	if err != nil {
		logger.Println(err)
	}
}
