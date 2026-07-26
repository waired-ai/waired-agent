package main

import (
	"bufio"
	"io"
)

// lineReader is the read side of every `waired init` prompt — the two
// bufio.Scanner methods the prompts actually use. Widening them to this
// interface is what lets the daemon-driven journey hand them the stdin
// owner below while every other caller keeps passing its own scanner,
// unchanged.
type lineReader interface {
	Scan() bool
	Text() string
}

// stdinReader is the single owner of stdin for one `waired init` run.
//
// The daemon-driven journey has two things that want the keyboard at
// overlapping times: the "press Enter to continue in the terminal
// instead" takeover offer, which watches for a keystroke while a
// multi-GB model downloads, and the ordinary prompts that follow it.
// They used to be two goroutines calling Scan on ONE bufio.Scanner —
// a data race whose visible symptom was theft: FIFO ordering handed the
// keystroke to whichever read was already parked, so an answer meant
// for the coding-agent question was routinely eaten by the still-live
// takeover watch (#185), and an Enter pressed at the sign-in step
// silently became a mode switch (#184).
//
// Here exactly one goroutine ever reads. It scans lines and publishes
// them on a channel; prompts consume with Scan/Text (blocking, so this
// substitutes for their scanner) and the takeover watch consumes with
// Poll (non-blocking, so the download bar keeps rendering). Two
// consumers, one reader, one queue — no race, and every line is
// delivered to exactly one waiting caller.
//
// Construct one ONLY for a real terminal. A piped stdin belongs to the
// script driving init, and reading ahead from it would swallow bytes
// meant for a later command in that script; off a TTY the prompts keep
// their own on-demand scanner and the takeover watch stays inert.
type stdinReader struct {
	lines <-chan string
	text  string // last line handed out by Scan, for Text
}

// stdinQueueDepth is how many typed-ahead lines the owner buffers before
// the reader goroutine blocks. Deep enough that a fast typist is never
// throttled, shallow enough that back-pressure leaves the rest of the
// input in the OS buffer where a Discard cannot reach past it.
const stdinQueueDepth = 8

// newStdinReader starts the one reader goroutine over r. It runs for the
// life of the process: there is no portable way to interrupt a blocking
// read on stdin, and `waired init` exits soon after its last prompt.
func newStdinReader(r io.Reader) *stdinReader {
	ch := make(chan string, stdinQueueDepth)
	sc := bufio.NewScanner(r)
	go func() {
		defer close(ch)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return &stdinReader{lines: ch}
}

// Scan blocks for the next line and reports false once stdin is
// exhausted — bufio.Scanner semantics, so a prompt cannot tell the
// difference.
func (s *stdinReader) Scan() bool {
	if s == nil {
		return false
	}
	line, ok := <-s.lines
	if !ok {
		s.text = ""
		return false
	}
	s.text = line
	return true
}

// Text returns the line the last Scan read.
func (s *stdinReader) Text() string {
	if s == nil {
		return ""
	}
	return s.text
}

// Poll returns a line only if one has already been typed, and never
// blocks — so a caller redrawing a progress bar can check the keyboard
// every tick. ok=false means "nothing typed yet", and after EOF it stays
// false forever: a scripted stdin that ran out of lines must not read as
// a keystroke.
func (s *stdinReader) Poll() (string, bool) {
	if s == nil {
		return "", false
	}
	select {
	case line, ok := <-s.lines:
		if !ok {
			return "", false
		}
		return line, true
	default:
		return "", false
	}
}

// Discard drops the lines that have already been typed. Callers use it
// immediately before arming a watch, or before a question whose answer
// must be deliberate, so a keystroke aimed at an earlier step cannot
// answer a later one (#184) — the terminal equivalent of a tcflush.
//
// It is best-effort by nature (a keystroke can always land a microsecond
// later) and it is why the takeover needs an affirmative answer rather
// than trusting the flush alone. It is only ever correct on a terminal,
// which is the only place a stdinReader exists.
func (s *stdinReader) Discard() {
	if s == nil {
		return
	}
	for {
		select {
		case _, ok := <-s.lines:
			if !ok {
				return // EOF: nothing more will arrive
			}
		default:
			return
		}
	}
}
