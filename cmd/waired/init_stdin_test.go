package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// pollLine waits for one already-typed line to reach the owner's queue.
// Poll is deliberately non-blocking, so tests have to give the reader
// goroutine a moment to publish.
func pollLine(t *testing.T, s *stdinReader) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if line, ok := s.Poll(); ok {
			return line
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for a line to reach the stdin owner")
	return ""
}

func TestStdinReaderScanTextIsFIFO(t *testing.T) {
	s := newStdinReader(strings.NewReader("first\nsecond\n"))
	for _, want := range []string{"first", "second"} {
		if !s.Scan() {
			t.Fatalf("Scan returned false before %q", want)
		}
		if got := s.Text(); got != want {
			t.Fatalf("Text = %q, want %q", got, want)
		}
	}
	if s.Scan() {
		t.Fatalf("Scan returned a third line: %q", s.Text())
	}
	if got := s.Text(); got != "" {
		t.Errorf("Text after EOF = %q, want empty", got)
	}
}

// TestStdinReaderPollDoesNotBlock is the property the takeover watch
// depends on: a caller rendering a download bar checks for a keystroke
// every tick and must never stall on an idle keyboard.
func TestStdinReaderPollDoesNotBlock(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	s := newStdinReader(pr)

	if line, ok := s.Poll(); ok {
		t.Fatalf("Poll on an idle keyboard returned %q", line)
	}
	if _, err := pw.Write([]byte("t\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	if got := pollLine(t, s); got != "t" {
		t.Errorf("Poll = %q, want %q", got, "t")
	}
	if line, ok := s.Poll(); ok {
		t.Errorf("second Poll returned %q, want nothing left", line)
	}
}

// TestStdinReaderDiscardDropsTypeahead covers #184: an Enter pressed at
// the sign-in step must not survive to answer a later question.
func TestStdinReaderDiscardDropsTypeahead(t *testing.T) {
	s := newStdinReader(strings.NewReader("\n\ny\n"))
	// Wait for the reader to publish at least the first line, so Discard
	// has something to drop rather than racing an empty queue.
	_ = pollLine(t, s)
	s.Discard()

	// Nothing typed before the Discard may reach a caller afterwards. The
	// reader goroutine may still be in flight, so give it a moment and
	// then assert the queue is drained rather than reordered.
	time.Sleep(50 * time.Millisecond)
	s.Discard()
	if line, ok := s.Poll(); ok {
		t.Errorf("Poll after Discard returned %q, want nothing", line)
	}
}

// TestStdinReaderPollAndScanShareOneReader is the #185 regression bar:
// a watch polling and a prompt scanning are two CONSUMERS of one
// reader, not two readers. Each line is delivered exactly once, and
// `go test -race` proves there is no concurrent Scan.
func TestStdinReaderPollAndScanShareOneReader(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	s := newStdinReader(pr)

	if _, err := pw.Write([]byte("watched\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	if got := pollLine(t, s); got != "watched" {
		t.Fatalf("Poll = %q, want %q", got, "watched")
	}

	scanned := make(chan string, 1)
	go func() {
		if s.Scan() {
			scanned <- s.Text()
			return
		}
		scanned <- "<eof>"
	}()
	if _, err := pw.Write([]byte("prompted\n")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	select {
	case got := <-scanned:
		if got != "prompted" {
			t.Errorf("the prompt read %q, want %q", got, "prompted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the prompt never got its line")
	}
}

// A nil owner is the off-a-terminal case; every method must be a safe
// no-op so callers need no branch.
func TestStdinReaderNilIsInert(t *testing.T) {
	var s *stdinReader
	if s.Scan() {
		t.Error("nil owner reported a line")
	}
	if got := s.Text(); got != "" {
		t.Errorf("nil owner Text = %q", got)
	}
	if line, ok := s.Poll(); ok {
		t.Errorf("nil owner Poll = %q", line)
	}
	s.Discard() // must not panic
}
