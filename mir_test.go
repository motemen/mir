package main

import "testing"

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
)

func TestMir_Smoke(t *testing.T) {
	upstreamBase, err := ioutil.TempDir("", "mir-test-upstream")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(upstreamBase)

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

	gitPort, err := emptyPort()
	if err != nil {
		t.Fatal(err)
	}

	gitDaemon := exec.Command("git", "daemon", "--verbose", "--export-all", "--base-path="+upstreamBase, "--port="+fmt.Sprintf("%d", gitPort), "--reuseaddr")
	gitDaemon.Stderr = os.Stderr
	defer func() {
		gitDaemon.Process.Signal(os.Interrupt)
		gitDaemon.Wait()
	}()

	err = gitDaemon.Start()
	if err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(upstreamBase, "foo", "bar.git")
	err = exec.Command("git", "init", "--bare", repoDir).Run()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "--work-tree", ".", "--git-dir", repoDir, "commit", "--allow-empty", "-m", "msg")
	cmd.Stderr = os.Stderr
	cmd.Env = []string{
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	}

	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	mir := server{
		basePath: mirBase,
		upstream: fmt.Sprintf("git://localhost:%d/", gitPort),
	}

	s := httptest.NewServer(&mir)
	defer s.Close()

	cmd = exec.Command("git", "clone", s.URL+"/foo/bar.git")
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
