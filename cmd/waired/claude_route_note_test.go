package main

import (
	"strings"
	"testing"
)

// waired-agent#812: the note explaining a cleared subagent pin used to be
// printed after the whole routing block, so on real hardware it landed
// below `waired node:` — one line further from what it explains than it
// needed to be. Measured on sv-mag running
// 0.0.2-edge.20260814180252+749e6a3:
//
//	main conversation:  auto  (…)
//	subagents:          same as main  (…)
//	waired node:        auto (this device or a mesh peer)   (…)
//	                    (subagents were pinned to waired — cleared. …)
//
// Nothing was misread — the sentence names the class it is about — so this
// pins placement, not comprehensibility.
func TestPrintClaudeRoutingState_PinNoteFollowsTheSubagentsLine(t *testing.T) {
	// An unreachable management address: claudeWairedNodeLine best-efforts
	// to "" and the `waired node:` line is simply absent, which is why the
	// body below is enough to drive the whole print path.
	const unreachable = "http://127.0.0.1:1"
	body := []byte(`{"policy":{"main":"auto","sub":"same"}}`)

	out := captureStdout(t, func() {
		if err := printClaudeRoutingState(unreachable, body, "waired"); err != nil {
			t.Fatalf("printClaudeRoutingState: %v", err)
		}
	})

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	subIdx, noteIdx := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "subagents:"):
			subIdx = i
		case strings.Contains(l, "were pinned to"):
			noteIdx = i
		}
	}
	if subIdx < 0 {
		t.Fatalf("no subagents line in:\n%s", out)
	}
	if noteIdx < 0 {
		t.Fatalf("the cleared-pin note was not printed:\n%s", out)
	}
	if noteIdx != subIdx+1 {
		t.Errorf("note is at line %d and `subagents:` at %d — it must be the next line:\n%s",
			noteIdx, subIdx, out)
	}
	// Both halves the reader needs: which pin went away, and how to get it
	// back. Asserted here as well as on the string builder, because this is
	// the path a user actually sees.
	if !strings.Contains(lines[noteIdx], "--sub waired") {
		t.Errorf("note does not say how to restore the pin: %q", lines[noteIdx])
	}
}

// The note must not appear when nothing was cleared — every other route
// command prints this same block.
func TestPrintClaudeRoutingState_NoNoteWhenNothingWasCleared(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printClaudeRoutingState("http://127.0.0.1:1",
			[]byte(`{"policy":{"main":"auto","sub":"waired"}}`), ""); err != nil {
			t.Fatalf("printClaudeRoutingState: %v", err)
		}
	})
	if strings.Contains(out, "were pinned to") {
		t.Errorf("a note appeared for a command that cleared nothing:\n%s", out)
	}
	if !strings.Contains(out, "subagents:") {
		t.Errorf("the routing block itself is missing:\n%s", out)
	}
}
