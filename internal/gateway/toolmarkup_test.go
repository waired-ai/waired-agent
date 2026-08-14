package gateway

import (
	"strings"
	"testing"
)

// The whole-turn markup verdict (waired-agent#786).
//
// Measured case: `claude -p 'Reply with exactly: PONG' --model qwen3.5-2b`
// against a locally-served qwen3.5-2b exited 0 in 73.8 s having printed
// only `<response>` / `</function>` / `</tool_call>`. Two 200 responses
// in the journal, nothing recovered, and the usable-turn check saw text
// and passed it.
//
// These pin what counts as markup-only and — more important — what does
// NOT. A false positive here appends a failure note to a turn that was
// fine, so the negative cases carry the weight.

func TestTextIsOnlyToolMarkup(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		// The measured turn, verbatim.
		{"the measured qwen3.5-2b turn", "<response>\n</function>\n</tool_call>\n", true},
		{"a lone closing tool_call", "</tool_call>", true},
		{"an opening call tag with an attribute", "<function=Bash>", true},
		{"the pipe dialect", "<|tool_call|>", true},
		{"the delimiter dialect", "[TOOL_CALLS]", true},
		{"markup inside a json fence", "```json\n<tool_call>\n</tool_call>\n```", true},
		{"parameters and whitespace", "  <parameter=path>\n\n</parameter>  ", true},

		// Not markup-only. Each of these must keep streaming as a normal
		// turn: appending a failure note to any of them would be a
		// regression the client sees.
		{"an empty turn is a different fault", "", false},
		{"whitespace only is a different fault", "   \n\t", false},
		{"the answer the prompt asked for", "PONG", false},
		{"an echoed prompt is not our call to make", "Reply with exactly: pong", false},
		{"prose that mentions a tag", "The closing tag is </tool_call> in that dialect.", false},
		{"prose after markup", "</tool_call>\nHere is the answer: 42", false},
		{"markup after prose", "Here is the answer: 42\n</function>", false},
		{"html in a code fence", "```\n<div class=\"x\">hi</div>\n```", false},
		{"a bare fence with code", "```\nprint(1)\n```", false},
		{"an unrelated tag alone", "<div>", false},
		{"json a recovery pass would have taken", `{"name":"Bash","arguments":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := textIsOnlyToolMarkup(tc.in); got != tc.want {
				t.Errorf("textIsOnlyToolMarkup(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestMarkupWatch_StopsAtTheCap: a long turn is not this failure mode,
// and the watch must not hold one in memory to say so.
func TestMarkupWatch_StopsAtTheCap(t *testing.T) {
	w := newMarkupWatch()
	w.add(strings.Repeat("a", markupWatchCap))
	w.add("</tool_call>")
	if w.onlyToolMarkup() {
		t.Error("a turn past the cap was reported as markup-only")
	}
	if len(w.seen) != 0 {
		t.Errorf("watch still holds %d bytes past the cap", len(w.seen))
	}
}

// TestMarkupWatch_AccumulatesAcrossDeltas: the tags arrive one SSE delta
// at a time, so the verdict has to be about the assembled turn. Judging
// each delta on its own is how a split tag would be missed.
func TestMarkupWatch_AccumulatesAcrossDeltas(t *testing.T) {
	w := newMarkupWatch()
	for _, d := range []string{"<resp", "onse>\n", "</fun", "ction>\n", "</tool_call>"} {
		w.add(d)
	}
	if !w.onlyToolMarkup() {
		t.Errorf("assembled turn %q was not reported as markup-only", string(w.seen))
	}
}

// TestMarkupWatch_NilIsSafe: the watch is created per stream; a nil one
// must not change any verdict.
func TestMarkupWatch_NilIsSafe(t *testing.T) {
	var w *markupWatch
	w.add("</tool_call>")
	if w.onlyToolMarkup() {
		t.Error("a nil watch reported markup-only")
	}
}
