package main

import "testing"

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/groupcache/lru"
	"github.com/pkg/errors"
)

var gitDaemon *gitDaemonSpec

type gitDaemonSpec struct {
	basePath string
	port     int
	cmd      *exec.Cmd
}

func (d gitDaemonSpec) cleanup() {
	os.RemoveAll(d.basePath)
	d.cmd.Process.Signal(os.Interrupt)
	d.cmd.Wait()
}

func (d gitDaemonSpec) addRepo(path string) (repo upstreamRepo, err error) {
	repo = upstreamRepo(filepath.Join(d.basePath, path+".git"))
	err = exec.Command("git", "init", "--bare", string(repo)).Run()
	if err != nil {
		return
	}
	err = repo.addNewCommit()
	return
}

type upstreamRepo string

func (r upstreamRepo) addNewCommit() error {
	tempdir, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}

	filename := filepath.Join(tempdir, fmt.Sprint(time.Now().UnixNano()))
	f, err := os.Create(filename)
	if err != nil {
		return err
	}

	for i := 0; i < 100; i++ {
		fmt.Fprint(f, time.Now().UnixNano())
	}
	f.Close()

	if err := runCommand("git", "--work-tree", tempdir, "--git-dir", string(r), "add", filename); err != nil {
		return err
	}

	return runCommand("git", "--work-tree", tempdir, "--git-dir", string(r), "commit", "-m", "msg")
}

func startGitDaemon() (daemon *gitDaemonSpec, err error) {
	d := gitDaemonSpec{}

	d.basePath, err = ioutil.TempDir("", "mir-test-daemon-base")
	if err != nil {
		return
	}

	d.port, err = emptyPort()
	if err != nil {
		return
	}

	d.cmd = exec.Command("git", "daemon", "--verbose", "--export-all", "--base-path="+d.basePath, "--port="+fmt.Sprintf("%d", d.port), "--reuseaddr")

	e, err := d.cmd.StderrPipe()
	if err != nil {
		return
	}

	err = d.cmd.Start()
	if err != nil {
		return
	}

	// Wait for git-daemon to start
	s := bufio.NewScanner(e)
	for s.Scan() {
		if strings.HasSuffix(s.Text(), "Ready to rumble") {
			break
		}
	}

	err = s.Err()
	if err != nil {
		return
	}

	return &d, nil
}

func TestMain(m *testing.M) {
	var err error
	gitDaemon, err = startGitDaemon()
	if err != nil {
		log.Fatal(err)
	}
	defer gitDaemon.cleanup()

	os.Exit(m.Run())
}

func runCommand(command string, args ...string) error {
	var buf bytes.Buffer
	cmd := exec.Command(command, args...)
	cmd.Stderr = &buf
	return errors.Wrapf(cmd.Run(), "%s %v: %s", command, args, buf.String())
}

func TestMir_Smoke(t *testing.T) {
	logger.SetOutput(ioutil.Discard)

	workTreeBase, err := ioutil.TempDir("", "mir-test-worktree")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		t.Log("removing worktree")
		os.RemoveAll(workTreeBase)
	}()

	mirBase, err := ioutil.TempDir("", "mir-test-base")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		t.Log("removing mir base")
		os.RemoveAll(mirBase)
	}()

	repo, err := gitDaemon.addRepo("foo/bar")
	if err != nil {
		t.Fatal(err)
	}

	mir := server{
		basePath:     mirBase,
		upstream:     fmt.Sprintf("git://localhost:%d/", gitDaemon.port),
		refsFreshFor: 50 * time.Millisecond,
	}
	mir.packCache.Cache = lru.New(20)

	s := httptest.NewServer(&mir)
	defer s.Close()

	duration := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	go func() {
		for i := 1; ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			time.Sleep(time.Millisecond * 500)
			err := repo.addNewCommit()
			if err != nil {
				t.Fatalf("addNewCommit(%d): %s", i, err)
			}
		}
	}()

	sem := make(chan struct{}, 100)
	var count int32
	var wg sync.WaitGroup
FOR:
	for {
		select {
		case <-ctx.Done():
			break FOR
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer func() { _ = <-sem }()
			defer wg.Done()

			count := atomic.AddInt32(&count, 1)

			wd := filepath.Join(workTreeBase, fmt.Sprint(count))
			if err := os.Mkdir(wd, 0755); err != nil {
				t.Fatal(err)
			}

			if err := runCommand("git", "clone", "--quiet", s.URL+"/foo/bar.git", wd); err != nil {
				t.Fatal(err)
			}
		}()
	}

	wg.Wait()

	fmt.Printf("Processed %d clones in %s\n", count, duration)
	fmt.Printf("syncSkipped: %d\n", syncSkipped.Value())
	fmt.Printf("packCacheHit: %d\n", packCacheHit.Value())
}

func emptyPort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	err = l.Close()
	if err != nil {
		return 0, err
	}

	return l.Addr().(*net.TCPAddr).Port, nil
}
