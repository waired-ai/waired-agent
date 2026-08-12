package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/download"
)

// writePrompt / writePromptf wrap fmt.Fprintln/Fprintf so the call
// sites stay terse and we don't repeatedly discard the error returned
// by writing to a terminal Writer — terminal write failures here are
// not actionable.
//
// They are also the one place the init flow's prose passes through, so the
// ASCII fold lives here (plainText, ascii.go): when the sink cannot render
// glyphs, em dashes and ellipses degrade with the emoji instead of being
// written as UTF-8 bytes onto a console that will decode them as CP932 (#629).
func writePrompt(out io.Writer, args ...any) {
	_, _ = fmt.Fprint(out, plainText(fmt.Sprintln(args...)))
}

func writePromptf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprint(out, plainText(fmt.Sprintf(format, args...)))
}

// ynAnswer is what a [Y/n] prompt got back. ynNoAnswer is the case
// ynPrompt cannot express: stdin ended before an answer arrived, so
// nobody is at the keyboard.
type ynAnswer int

const (
	ynYes ynAnswer = iota
	ynNo
	ynNoAnswer
)

// ynPrompt reads one [Y/n] / [y/N] answer. Empty input returns def.
// Unparseable input re-prompts up to 3 times then falls back to def.
// Reads through the supplied line source (caller owns it) — a plain
// bufio.Scanner, or the init stdin owner when one process-wide reader is
// what keeps two prompts from fighting over the keyboard (init_stdin.go).
//
// An exhausted stdin also returns def, which is what an unattended
// install wants from a configuration question: take the documented
// default and carry on. A question whose Yes REPLACES a working setup or
// DELETES data wants the opposite, and reads ynAsk directly — see
// init_benchmark.go (waired-agent#754).
func ynPrompt(out io.Writer, sc lineReader, label string, def bool) bool {
	switch ynAsk(out, sc, label, def) {
	case ynYes:
		return true
	case ynNo:
		return false
	default: // ynNoAnswer — unchanged from before ynAsk existed.
		return def
	}
}

// ynAsk is ynPrompt with the no-answer case kept apart.
//
// Empty input is still an answer — a person pressed Enter, and taking
// def is the whole point of printing one. A closed stdin is not: nobody
// is there to press it. `waired init` does not force non-interactive
// mode off a terminal (it cannot: scripted installs pipe their answers
// in, and scripts/dev/installtest-windows.ps1 drives it that way), so
// EOF was reaching the benchmark flow's default-Yes prompts and
// switching the model, then offering to delete the weights it moved off
// — with no one to see either question.
func ynAsk(out io.Writer, sc lineReader, label string, def bool) ynAnswer {
	// Spell out the default ("default: Yes/No") alongside the [Y/n]
	// capitalization so it reads like a conventional interactive installer
	// — the older "(Enter = Yes)" form looked like an instruction to type
	// the word "Yes".
	hint := "[Y/n] (default: Yes)"
	if !def {
		hint = "[y/N] (default: No)"
	}
	fromDefault := func() ynAnswer {
		if def {
			return ynYes
		}
		return ynNo
	}
	for range 3 {
		writePromptf(out, "  %s %s ", label, hint)
		if !sc.Scan() {
			return ynNoAnswer
		}
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		switch line {
		case "":
			return fromDefault()
		case "y", "yes":
			return ynYes
		case "n", "no":
			return ynNo
		}
		writePrompt(out, "  please answer y or n.")
	}
	// Three unparseable answers: somebody IS there, they are just not
	// answering the question. That is the pre-existing fall back to def,
	// not a no-answer.
	return fromDefault()
}

// downloadLineState carries the throttling state between drawDownloadLine
// calls (last redraw time and last rendered percentage).
type downloadLineState struct {
	lastDraw time.Time
	lastPct  int
}

// drawDownloadLine renders one aggregated model-download progress line, e.g.:
//
//	⬇️  Downloading qwen2.5-coder-7b-instruct: 45%  2.3 GB / 5.0 GB (40.0 MB/s)
//
// speed is the smoothed transfer rate: rendered whenever it is known (>= 0)
// — including "(0 B/s)", which tells a stalled transfer apart from a frozen
// UI — and omitted only while still unknown (< 0, before the first rate
// sample). The byte counts alone tick too coarsely (0.1 GB steps) to prove
// liveness on a slow link. On a TTY the line is rewritten in place (\r),
// time-throttled to ~150 ms so the speed stays lively; off a TTY (piped
// install logs) it emits a fresh line per ~10% — or per ~5 s when the total
// is unknown — so logs stay readable without \r spam. st carries the
// throttle state between calls. Shared by cliPullProgressSink (Deploy's
// foreground pre-pull), waitForBundledModel (init's post-start /status
// wait), and the `runtimes install ollama` tarball download so all render
// identically.
func drawDownloadLine(out io.Writer, tty bool, st *downloadLineState, model string, pct int, completed, total, speed int64) {
	const throttle = 150 * time.Millisecond
	const ttyBucket = 10               // non-tty: redraw per 10%
	const plainEvery = 5 * time.Second // non-tty cadence when pct is unknown
	now := time.Now()
	switch {
	case tty:
		if !st.lastDraw.IsZero() && now.Sub(st.lastDraw) < throttle {
			return
		}
	case pct >= 0:
		if st.lastPct >= 0 && pct/ttyBucket == st.lastPct/ttyBucket {
			return
		}
	default: // non-tty with no percentage to bucket on: fall back to time
		if !st.lastDraw.IsZero() && now.Sub(st.lastDraw) < plainEvery {
			return
		}
	}
	st.lastDraw = now
	st.lastPct = pct
	label := emo("⬇️", "[..]")
	rate := ""
	if speed >= 0 {
		rate = fmt.Sprintf(" (%s/s)", download.HumanBytes(speed))
	}
	var line string
	switch {
	case total > 0:
		line = fmt.Sprintf("%s  Downloading %s: %3d%%  %s / %s%s",
			label, model, pct, download.HumanBytes(completed), download.HumanBytes(total), rate)
	case completed > 0: // length unknown (no Content-Length): bytes so far
		line = fmt.Sprintf("%s  Downloading %s: %s%s",
			label, model, download.HumanBytes(completed), rate)
	default:
		line = fmt.Sprintf("%s  Downloading %s…", label, model)
	}
	if tty {
		// Pad to clear any residue from a longer previous line; no trailing
		// newline so the next draw overwrites in place.
		writePromptf(out, "\r%-78s", line)
	} else {
		writePrompt(out, line)
	}
}
