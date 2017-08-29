package main

import (
	"io"
	"log"
	"os/exec"
	"time"

	"github.com/motemen/go-nuts/logwriter"
)

// repoCommand is a command associated with a specific repository.
type repoCommand struct {
	repo   *repository
	cmd    *exec.Cmd
	logger *log.Logger
}

func (c repoCommand) run() error {
	cmd := c.cmd

	start := time.Now()
	c.logger.Printf("[command %p] %q starting", cmd, cmd.Args)
	defer func() {
		elapsed := time.Now().Sub(start)
		elapsed = elapsed / 1000 * 1000
		c.logger.Printf("[command %p] %q finished (%s)", cmd, cmd.Args, elapsed)
	}()

	for _, s := range []struct {
		writer *io.Writer
		name   string
	}{
		{&cmd.Stdout, "out"},
		{&cmd.Stderr, "err"},
	} {
		if *s.writer != nil {
			continue
		}

		w := &logwriter.LogWriter{
			Logger:     c.logger,
			Format:     "[command %p :: %s] %s",
			FormatArgs: []interface{}{cmd, s.name},
		}

		*s.writer = w

		defer w.Close()
	}

	err := cmd.Start()
	if err != nil {
		return err
	}

	return cmd.Wait()
}
