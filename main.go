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

	"github.com/golang/groupcache/lru"
	"github.com/motemen/go-nuts/logwriter"
)

var logger = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)

// repository represents a repository that mir synchronizes.
// A *repository instance is unique by its path (under a *server),
// so calling its Lock() makes sense.
type repository struct {
	sync.RWMutex
	path             string
	upstreamURL      string
	localDir         string
	lastSynchronized time.Time
}

type server struct {
	upstream string
	basePath string

	repos struct {
		sync.Mutex
		m map[string]*repository
	}

	packCache    packCache
	refsFreshFor time.Duration
}

func (s *server) repository(repoPath string) *repository {
	s.repos.Lock()
	defer s.repos.Unlock()

	if s.repos.m == nil {
		s.repos.m = map[string]*repository{}
	}

	repoPath = strings.TrimSuffix(repoPath, ".git")

	repo, ok := s.repos.m[repoPath]
	if !ok {
		repo = &repository{
			path:        repoPath,
			upstreamURL: s.upstream + repoPath,
			// TODO(motemen): escape special characters
			localDir: filepath.Join(append([]string{s.basePath}, strings.Split(repoPath, "/")...)...),
		}
		s.repos.m[repoPath] = repo
	}

	return repo
}

type packCache struct {
	sync.Mutex
	*lru.Cache
}

func (c *packCache) key(repo *repository, clientRequest []byte) string {
	reqDigest := sha1.Sum(clientRequest)
	return repo.path + "\000" + string(reqDigest[:])
}

func (c *packCache) Get(repo *repository, clientRequest []byte) []byte {
	c.Lock()
	defer c.Unlock()

	key := c.key(repo, clientRequest)
	if v, ok := c.Cache.Get(key); ok {
		return v.([]byte)
	} else {
		return nil
	}
}

func (c *packCache) Add(repo *repository, clientRequest []byte, data []byte) {
	c.Lock()
	defer c.Unlock()

	key := c.key(repo, clientRequest)
	c.Cache.Add(key, data)
}

type repoLock struct {
	sync.RWMutex
	lastSynchronized time.Time
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

func (s *server) synchronizeCache(repo *repository) error {
	repo.Lock()
	defer repo.Unlock()

	if time.Now().Before(repo.lastSynchronized.Add(s.refsFreshFor)) {
		logger.Printf("[repo %s] Refs last synchronized at %s, not synchronizing repo", repo.path, repo.lastSynchronized)
		return nil
	}

	fi, err := os.Stat(repo.localDir)
	if err != nil {
		if os.IsNotExist(err) {
			// cache does not exist, so initialize one (may take long)
			if err := os.MkdirAll(repo.localDir, 0777); err != nil {
				return err
			}

			cmd := exec.Command("git", "clone", "--verbose", "--mirror", repo.upstreamURL, ".")
			cmd.Dir = repo.localDir
			err := runCommandLogged(cmd)
			if err == nil {
				repo.lastSynchronized = time.Now()
			} else {
				os.Remove(repo.localDir)
			}
			return err
		}

		return err
	} else if fi != nil && fi.IsDir() {
		// cache exists, update it
		// TODO(motemen): check the directory is a valid git repository
		cmd := exec.Command("git", "remote", "--verbose", "update")
		cmd.Dir = repo.localDir
		err := runCommandLogged(cmd)
		if err == nil {
			repo.lastSynchronized = time.Now()
		}
		return err
	}

	return fmt.Errorf("could not synchronize cache: %v", repo)
}

func (s *server) advertiseRefs(repo *repository, w http.ResponseWriter) {
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

	repo.RLock()
	defer repo.RUnlock()

	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", ".")
	cmd.Stdout = w
	cmd.Dir = repo.localDir
	err := runCommandLogged(cmd)
	if err != nil {
		logger.Println(err)
	}
}

func (s *server) uploadPack(repo *repository, w http.ResponseWriter, r io.ReadCloser) {
	repo.RLock()
	defer repo.RUnlock()

	clientRequest, err := ioutil.ReadAll(r)
	r.Close()

	if err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	if packResponse := s.packCache.Get(repo, clientRequest); packResponse != nil {
		resp := make([]byte, len(packResponse))
		copy(resp, packResponse)

		message := "[mir] Using cached pack\n"
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
	cmd.Dir = repo.localDir
	if err := runCommandLogged(cmd); err != nil {
		logger.Println(err)
		return
	}

	s.packCache.Add(repo, clientRequest, respBody.Bytes())
	io.Copy(w, &respBody)
}

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	logger.Printf("[request %p] %s %s %v", req, req.Method, req.URL, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// mode: ref delivery
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/info/refs")
		repo := s.repository(repoPath)

		s.advertiseRefs(repo, w)
	} else if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/git-upload-pack") {
		// mode: upload-pack
		repoPath := strings.TrimSuffix(req.URL.Path[1:], "/git-upload-pack")
		repo := s.repository(repoPath)

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

	s.refsFreshFor = 5 * time.Second // TODO make configurable, say "--refs-fresh-for="
	s.packCache.Cache = lru.New(20)  // TODO make configurable, say "--num-pack-cache="

	logger.Printf("[server %p] mir starting at %s ...", &s, listen)

	err := http.ListenAndServe(listen, &s)
	if err != nil {
		logger.Println(err)
	}
}
