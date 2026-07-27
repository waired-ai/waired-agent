package main

import (
	"strings"
	"testing"
	"time"
)

// pollWatch drives the watch until it has something to say, the way the
// foreground waits do (once per poll tick).
func pollWatch(t *testing.T, w *enterWatch) (bool, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if took, note := w.Poll(); took || note != "" {
			return took, note
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the takeover watch to react")
	return false, ""
}

// TestTakeoverWatchEnterAsksFirst is the #184 regression bar: a bare
// Enter — the keystroke the sign-in step above teaches — must never
// switch modes on its own. It asks, and says what it is asking.
func TestTakeoverWatchEnterAsksFirst(t *testing.T) {
	w := newTakeoverWatch(newStdinReader(strings.NewReader("\n")))
	took, note := pollWatch(t, w)
	if took {
		t.Fatal("a bare Enter took over the terminal without confirmation")
	}
	if !strings.Contains(note, "Take over setup in this terminal?") {
		t.Errorf("note does not ask for confirmation: %q", note)
	}
	if !strings.Contains(note, "browser page stops") {
		t.Errorf("note does not say what taking over does: %q", note)
	}
	if w.Fired() {
		t.Error("Fired reported true before an answer")
	}
}

func TestTakeoverWatchAffirmativeTakesOver(t *testing.T) {
	w := newTakeoverWatch(newStdinReader(strings.NewReader("\ny\n")))
	if took, _ := pollWatch(t, w); took {
		t.Fatal("took over on the question itself")
	}
	took, note := pollWatch(t, w)
	if !took {
		t.Fatalf("`y` did not confirm the takeover (note=%q)", note)
	}
	if !w.Fired() {
		t.Error("Fired is false after a confirmed takeover")
	}
	// Sticky: later polls keep reporting the takeover and stop talking.
	if took, note := w.Poll(); !took || note != "" {
		t.Errorf("post-takeover Poll = (%v, %q), want (true, \"\")", took, note)
	}
}

// A second bare Enter answers the question with its default (No), so
// the muscle-memory double-tap that motivated #184 cannot take over.
func TestTakeoverWatchSecondEnterDeclines(t *testing.T) {
	w := newTakeoverWatch(newStdinReader(strings.NewReader("\n\n")))
	if took, _ := pollWatch(t, w); took {
		t.Fatal("took over on the question itself")
	}
	took, note := pollWatch(t, w)
	if took {
		t.Fatal("a bare Enter answered the [y/N] question affirmatively")
	}
	if !strings.Contains(note, "Continuing in your browser") {
		t.Errorf("declining note = %q", note)
	}
	if w.Fired() {
		t.Error("Fired is true after declining")
	}
}

// After declining, the offer is still live — the operator can change
// their mind while the download runs.
func TestTakeoverWatchReArmsAfterDecline(t *testing.T) {
	w := newTakeoverWatch(newStdinReader(strings.NewReader("\nn\n\ny\n")))
	pollWatch(t, w) // question
	if took, _ := pollWatch(t, w); took {
		t.Fatal("`n` took over")
	}
	pollWatch(t, w) // question again
	if took, _ := pollWatch(t, w); !took {
		t.Fatal("`y` after a declined round did not take over")
	}
}

// EOF never takes over: a scripted stdin that ran out of lines keeps the
// wait in the foreground, which is what makes piped input deterministic.
func TestTakeoverWatchEOFIsSilent(t *testing.T) {
	w := newTakeoverWatch(newStdinReader(strings.NewReader("")))
	time.Sleep(50 * time.Millisecond)
	if took, note := w.Poll(); took || note != "" {
		t.Errorf("EOF produced (%v, %q), want silence", took, note)
	}
}

// Off a terminal there is no stdin owner, so the watch is inert and
// callers need no nil check of their own.
func TestTakeoverWatchInertWithoutOwner(t *testing.T) {
	for _, w := range []*enterWatch{nil, newTakeoverWatch(nil)} {
		if took, note := w.Poll(); took || note != "" {
			t.Errorf("inert watch produced (%v, %q)", took, note)
		}
		if w.Fired() {
			t.Error("inert watch reported a fired wait")
		}
	}
}
