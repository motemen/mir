package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"expvar"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/groupcache/lru"
)

var logger = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile|log.Lmicroseconds)

var (
	packCacheHit = expvar.NewInt("packCacheHit")
	syncSkipped  = expvar.NewInt("syncSkipped")
)

var version = "0.3.0"

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

func (repo *repository) gitCommand(args ...string) repoCommand {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo.localDir
	return repoCommand{
		repo:   repo,
		cmd:    cmd,
		logger: logger,
	}
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
	// experimental
	useCachePack bool
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

// synchronizeCache fetches Git content from upstream to synchronize local copy of repo.
// It does not synchronize if last synchronized time is within s.refsFreshFor from now.
func (s *server) synchronizeCache(repo *repository) error {
	repo.Lock()
	defer repo.Unlock()

	if time.Now().Before(repo.lastSynchronized.Add(s.refsFreshFor)) {
		syncSkipped.Add(1)
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

			gitClone := repo.gitCommand("clone", "--verbose", "--mirror", repo.upstreamURL, ".")
			err := gitClone.run()
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
		gitRemoteUpdate := repo.gitCommand("remote", "--verbose", "update")
		if err := gitRemoteUpdate.run(); err != nil {
			return err
		}
		repo.lastSynchronized = time.Now()
		return nil
	}

	return fmt.Errorf("could not synchronize cache: %v", repo)
}

// advertiseRefs sends the refs list to client.
// It roughly corresponds to "git ls-remote."
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

	// do not want to list refs while mirroring, so RLock
	repo.RLock()
	defer repo.RUnlock()

	gitUploadPack := repo.gitCommand("upload-pack", "--stateless-rpc", "--advertise-refs", ".")
	gitUploadPack.cmd.Stdout = w
	err := gitUploadPack.run()
	if err != nil {
		logger.Println(err)
	}

	// no need to return err, as the client knows if something goes wrong
}

// uploadPack sends the Git objects to client.
// Canonical Git implimentation does interactive negotiation,
// but for caching purpose this reads all the client's request body
// and then responds to it.
func (s *server) uploadPack(repo *repository, w http.ResponseWriter, r io.ReadCloser) {
	if err := s.synchronizeCache(repo); err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	repo.RLock()
	defer repo.RUnlock()

	if s.useCachePack == false {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")

		gitUploadPack := repo.gitCommand("upload-pack", "--stateless-rpc", ".")
		gitUploadPack.cmd.Stdout = w
		gitUploadPack.cmd.Stdin = r
		if err := gitUploadPack.run(); err != nil {
			logger.Println(err)
		}
		return
	}

	clientRequest, err := ioutil.ReadAll(r)
	defer r.Close()

	if err != nil {
		logger.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	buf := bytes.NewBuffer(clientRequest)
	// log client capabilities
	if pkt := newPktLineScanner(buf); pkt.Scan() {
		line := pkt.Text()
		// must be 'first-want'
		// https://github.com/git/git/blob/v2.7.1/Documentation/technical/pack-protocol.txt#L224
		if strings.HasPrefix(line, "want ") && len(line) > len("want ")+40 && line[len("want ")+40] == ' ' {
			capabilities := strings.Fields(line[len("want ")+40+1:])
			logger.Printf("client capabilities: %v", capabilities)
		} else {
			logger.Printf("warning: not a first-want pkt-line: %q", line)
		}
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	if packResponse := s.packCache.Get(repo, clientRequest); packResponse != nil {
		packCacheHit.Add(1)
		w.Write(packResponse)
		return
	}

	var respBody bytes.Buffer

	gitUploadPack := repo.gitCommand("upload-pack", "--stateless-rpc", ".")
	gitUploadPack.cmd.Stdout = &respBody
	gitUploadPack.cmd.Stdin = bytes.NewBuffer(clientRequest)
	if err := gitUploadPack.run(); err != nil {
		logger.Println(err)
		return
	}

	s.packCache.Add(repo, clientRequest, respBody.Bytes())
	io.Copy(w, &respBody)
}

var expvarHandler = expvar.Handler()

func (s *server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	logger.Printf("[request %p] %s %s %v", req, req.Method, req.URL, req.Header)

	if strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// mode: ref discovery
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
	} else if req.Method == "GET" && req.URL.Path == "/debug/vars" {
		expvarHandler.ServeHTTP(w, req)
	} else {
		http.Error(w, "Not Implemented", http.StatusNotImplemented)
	}
}

func main() {
	var (
		s            server
		listen       string
		numPackCache int
		printVersion bool
	)
	flag.StringVar(&s.upstream, "upstream", "", "upstream repositories' base `URL`")
	flag.StringVar(&s.basePath, "base-path", "", "base `directory` for locally cloned repositories")
	flag.StringVar(&listen, "listen", ":9280", "`address` to listen to")
	flag.DurationVar(&s.refsFreshFor, "refs-fresh-for", 5*time.Second, "`duration` to consider synchronized refs (keep this very short)")
	flag.IntVar(&numPackCache, "num-pack-cache", 20, "`number` of pack caches to keep in memory")
	flag.BoolVar(&printVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -listen=<addr> -upstream=<url> -base-path=<path>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if printVersion {
		fmt.Printf("mir %s\n", version)
		os.Exit(0)
	}

	if s.upstream == "" || s.basePath == "" {
		flag.Usage()
		os.Exit(2)
	}

	s.packCache.Cache = lru.New(numPackCache)

	logger.Printf("[server %p] mir %s starting at %s ...", &s, version, listen)

	err := http.ListenAndServe(listen, &s)
	if err != nil {
		logger.Println(err)
	}
}

func newPktLineScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Split(splitPktLine)
	return s
}

func splitPktLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && data == nil {
		return
	}

	var n int64
	n, err = strconv.ParseInt(string(data[0:4]), 16, 32)
	if err != nil {
		return
	}

	if n == 0 {
		advance = 4
		token = []byte{}
		return
	}

	advance = int(n)
	token = data[4:n]
	return
}
