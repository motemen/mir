package main

import (
	"bufio"
	"io"
	"log"
	"os/exec"
	"sync"
)

// repoCommand is a command associated with a specific repository.
type repoCommand struct {
	repo   *repository
	cmd    *exec.Cmd
	logger *log.Logger
}

func (c repoCommand) run() error {
	cmd := c.cmd

	c.logger.Printf("[command %p] %q starting", cmd, cmd.Args)
	defer c.logger.Printf("[command %p] %q finished", cmd, cmd.Args)

	var wg sync.WaitGroup

	errc := make(chan error, 2)

	for _, s := range []struct {
		writer io.Writer
		pipe   func() (io.ReadCloser, error)
		name   string
	}{
		{cmd.Stdout, cmd.StdoutPipe, "out"},
		{cmd.Stderr, cmd.StderrPipe, "err"},
	} {
		if s.writer != nil {
			continue
		}

		r, err := s.pipe()
		if err != nil {
			return err
		}

		wg.Add(1)
		go func(r io.ReadCloser, name string) {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				c.logger.Printf("[repo %q, command %p :: %s] %s", c.repo.path, cmd, name, scanner.Text())
				break
			}
			errc <- scanner.Err()
			wg.Done()
		}(r, s.name)
	}

	err := cmd.Start()
	if err != nil {
		return err
	}

	wg.Wait()

	close(errc)
	for e := range errc {
		if err == nil {
			err = e
		}
	}
	if err != nil {
		return err
	}

	return cmd.Wait()
}
