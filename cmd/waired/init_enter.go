package main

import (
	"bufio"
	"io"
)

// enterListener watches the shared stdin scanner for one line — the
// "press Enter to continue in the background" escape of the post-accept
// model-download wait (waired#774) — without corrupting the scanner for
// later prompts on the same stdin (the init flow asks the routing
// question after the benchmark).
//
// It deliberately reads a whole line via the scanner instead of a raw
// single-key read: raw terminal mode would fight bytes already buffered
// inside the shared scanner and needs per-OS state handling, while a
// line read is portable across linux/windows/darwin and scriptable in
// tests. The trade-off: when the wait finishes before the user pressed
// anything, the goroutine is still blocked in Scan — Drain then prompts
// for one Enter so the pending read cannot swallow the answer to the
// next prompt.
type enterListener struct {
	ch         chan bool // exactly one value: true = line read, false = EOF/error
	resolved   bool
	background bool
}

// listenForEnter starts a goroutine that performs exactly one sc.Scan()
// and reports the result. The caller MUST call Drain before issuing any
// further prompt on the same scanner.
func listenForEnter(sc *bufio.Scanner) *enterListener {
	l := &enterListener{ch: make(chan bool, 1)}
	go func() { l.ch <- sc.Scan() }()
	return l
}

// Backgrounded non-blockingly reports whether a line arrived; any content
// (or none) counts as "background me". EOF never backgrounds — a scripted
// stdin that ran out of lines keeps the wait in the foreground, which
// keeps piped-input behavior deterministic.
func (l *enterListener) Backgrounded() bool {
	if l.resolved {
		return l.background
	}
	select {
	case gotLine := <-l.ch:
		l.resolved = true
		l.background = gotLine
		return l.background
	default:
		return false
	}
}

// Drain reconciles the pending read once the wait is over: if the
// goroutine is still blocked in Scan, prompt for one Enter and block
// until it returns, so the pending read cannot eat the next prompt's
// answer. No-op when already resolved (Enter pressed or EOF).
func (l *enterListener) Drain(out io.Writer) {
	if l == nil || l.resolved {
		return
	}
	select {
	case <-l.ch:
		l.resolved = true
	default:
		writePrompt(out, "Press Enter to continue…")
		<-l.ch
		l.resolved = true
	}
}
