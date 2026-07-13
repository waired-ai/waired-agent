package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEnterListener_LineBackgrounds(t *testing.T) {
	l := listenForEnter(bufio.NewScanner(strings.NewReader("\n")))
	deadline := time.Now().Add(5 * time.Second)
	for !l.Backgrounded() {
		if time.Now().After(deadline) {
			t.Fatal("Enter line never observed")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEnterListener_EOFDoesNotBackground(t *testing.T) {
	l := listenForEnter(bufio.NewScanner(strings.NewReader("")))
	// Give the goroutine time to hit EOF, then confirm it never backgrounds
	// and Drain returns without prompting.
	time.Sleep(50 * time.Millisecond)
	if l.Backgrounded() {
		t.Fatal("EOF must not background the wait")
	}
	var out strings.Builder
	l.Drain(&out)
	if strings.Contains(out.String(), "Press Enter") {
		t.Errorf("EOF drain must not prompt, got: %q", out.String())
	}
}

func TestEnterListener_DrainBlocksUntilLine(t *testing.T) {
	pr, pw := io.Pipe()
	l := listenForEnter(bufio.NewScanner(pr))

	var out strings.Builder
	done := make(chan struct{})
	go func() {
		l.Drain(&out)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Drain returned before the pending Scan resolved")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := pw.Write([]byte("\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after the line arrived")
	}
	if !strings.Contains(out.String(), "Press Enter to continue") {
		t.Errorf("Drain should prompt for the reconciling Enter, got: %q", out.String())
	}
}
