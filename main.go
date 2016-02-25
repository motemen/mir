package main

import (
	"bytes"
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

	"compress/gzip"
	"net/http"
	"net/url"

	"github.com/motemen/go-nuts/broadcastwriter"
	"github.com/motemen/go-nuts/logwriter"
	"gopkg.in/src-d/go-git.v3"
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
		m map[string]*repoLock
	}
}

type repoLock struct {
	sync.RWMutex
	w *broadcastwriter.BroadcastWriter
}

func (l *repoLock) Unlock() {
	l.w.Close()
	l.w = broadcastwriter.NewBroadcastWriter()
	l.RWMutex.Unlock()
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
	// TODO(motemen): include protocols (e.g. "https") for completeness
	// Note that "ssh://user@example.com/foo/bar" and "user@example.com:foo/bar" differs
	// (git/Documentation/technical/pack-protocol.txt)
	path := append([]string{s.basePath, repo.URL.Host}, strings.Split(repo.URL.Path, "/")...)
	return filepath.Join(path...)
}

func (s *server) repoMutex(repo *upstreamRepo) *repoLock {
	s.repoMutexes.Lock()
	defer s.repoMutexes.Unlock()

	if s.repoMutexes.m == nil {
		s.repoMutexes.m = map[string]*repoLock{}
	}

	repoURL := repo.URL.String()
	mu, ok := s.repoMutexes.m[repoURL]
	if !ok {
		mu = &repoLock{
			w: broadcastwriter.NewBroadcastWriter(),
		}
		s.repoMutexes.m[repoURL] = mu
	}

	return mu
}

func runCommandLogged(cmd *exec.Cmd, writer io.Writer) error {
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
			var w io.Writer
			w = &logwriter.LogWriter{
				Logger:     logger,
				Format:     "[command %p :: %s] %s", // XXX if the command output contains "\r", the log becomes odd
				FormatArgs: []interface{}{cmd, s.name},
				Calldepth:  9,
			}
			if writer != nil {
				w = io.MultiWriter(w, writer)
			}
			*s.writer = w
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
			return runCommandLogged(cmd, mu.w)
		}

		return err
	} else if fi != nil && fi.IsDir() {
		// cache exists, update it
		cmd := exec.Command("git", "remote", "--verbose", "update")
		cmd.Dir = dir
		return runCommandLogged(cmd, mu.w)
	}

	return fmt.Errorf("cache could not synchronize: %v", repo)
}

func (s *server) advertiseRefs(repo *upstreamRepo, w http.ResponseWriter) {
	go s.synchronizeCache(repo)

	remote, err := git.NewRemote(repo.URL.String())
	if err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := remote.Connect(); err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	info := remote.Info()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Write(info.Bytes())
}

// XXX: Wanted to include "git remote update" progress into "git fetch"'s,
// but it must be interleaved into packfile, so we must finish the negotiation first to do that.
//
// To do so, we must have "git remote update" finished ... and it's a dead lock.
func (s *server) uploadPack(repo *upstreamRepo, w http.ResponseWriter, r io.ReadCloser) {
	mu := s.repoMutex(repo)

	// TODO: RLock repo
	l := mu.w.NewListener()
	for b := range l {
		logger.Printf("waiting %q", b)
	}
	// mu.RLock()
	// defer mu.RUnlock()

	clientRequest, err := ioutil.ReadAll(r)
	r.Close()

	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", ".")
	cmd.Stdout = w
	cmd.Stdin = bytes.NewBuffer(clientRequest)
	cmd.Dir = s.localDir(repo)
	if err := runCommandLogged(cmd, nil); err != nil {
		// TODO(motemen): send error to client
		logger.Println(err)
		return
	}
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
