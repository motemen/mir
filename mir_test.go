package main

import "testing"

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang/groupcache/lru"
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
	cmd := exec.Command("git", "--work-tree", ".", "--git-dir", string(r), "commit", "--allow-empty", "-m", "msg")
	cmd.Stderr = os.Stderr
	cmd.Env = []string{
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	}

	return cmd.Run()
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

func TestMir_Smoke(t *testing.T) {
	workTreeBase, err := ioutil.TempDir("", "mir-test-worktree")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workTreeBase)

	mirBase, err := ioutil.TempDir("", "mir-test-base")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(mirBase)

	_, err = gitDaemon.addRepo("foo/bar")
	if err != nil {
		t.Fatal(err)
	}

	mir := server{
		basePath:     mirBase,
		upstream:     fmt.Sprintf("git://localhost:%d/", gitDaemon.port),
		refsFreshFor: 5 * time.Second,
	}
	mir.packCache.Cache = lru.New(20)

	s := httptest.NewServer(&mir)
	defer s.Close()

	cmd := exec.Command("git", "clone", s.URL+"/foo/bar.git")
	cmd.Dir = workTreeBase
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
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
