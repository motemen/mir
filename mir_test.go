package main

import "testing"

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
)

func TestMir(t *testing.T) {
	upstreamBase, err := ioutil.TempDir("", "mir-test-upstream")
	if err != nil {
		t.Fatal(err)
	}

	workTreeBase, err := ioutil.TempDir("", "mir-test-worktree")
	if err != nil {
		t.Fatal(err)
	}

	mirBase, err := ioutil.TempDir("", "mir-test-base")
	if err != nil {
		t.Fatal(err)
	}

	gitDaemon := exec.Command("git", "daemon", "--verbose", "--export-all", "--base-path="+upstreamBase, "--reuseaddr")
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
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	mir := exec.Command("./mir", "-upstream", "git://localhost/", "-base-path", mirBase, "-listen", "localhost:9280")
	mir.Stderr = os.Stderr

	defer func() {
		mir.Process.Kill()
		mir.Wait()
	}()
	err = mir.Start()
	if err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command("git", "clone", "http://localhost:9280/foo/bar.git")
	cmd.Dir = workTreeBase
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}
