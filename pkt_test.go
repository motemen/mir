package main

import (
	"bufio"
	"bytes"
	"testing"
)

func nextScan(t *testing.T, s *bufio.Scanner, expected string) {
	t.Helper()

	if !s.Scan() {
		t.Fatal("got Scan() == false")
	}
	if got := s.Text(); got != expected {
		t.Errorf("got %q != %q", got, expected)
	}
}

func TestPktLineScanner(t *testing.T) {
	var buf bytes.Buffer
	s := bufio.NewScanner(&buf)
	s.Split(splitPktLine)
	buf.WriteString("003ewant 0ab1a827b3193d55b023c1051c6d00bb45057e46 no-progress\n")
	buf.WriteString("0000")
	buf.WriteString("0032have 136802d3c5782043066e192863c45c421b88f0a8\n")
	buf.WriteString("0009done\n")

	nextScan(t, s, "want 0ab1a827b3193d55b023c1051c6d00bb45057e46 no-progress\n")
	nextScan(t, s, "")
	nextScan(t, s, "have 136802d3c5782043066e192863c45c421b88f0a8\n")
	nextScan(t, s, "done\n")
	if s.Scan() == true {
		t.Fatalf("got Scan() == true, Text() = %q", s.Text())
	}
}
