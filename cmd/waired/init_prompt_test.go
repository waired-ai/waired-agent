package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestYnPrompt_UnparseableThenDefault(t *testing.T) {
	out := &bytes.Buffer{}
	sc := bufio.NewScanner(strings.NewReader("maybe\nperhaps\nwhat\n"))
	got := ynPrompt(out, sc, "Enable?", true)
	if !got {
		t.Errorf("got false, want default true after 3 bad answers")
	}
}

func TestYnPrompt_DefaultHintSpelledOut(t *testing.T) {
	// The hint must spell out the default ("default: Yes/No") so non-native
	// speakers aren't left to infer it from the [Y/n] capitalization alone.
	yes := &bytes.Buffer{}
	_ = ynPrompt(yes, bufio.NewScanner(strings.NewReader("\n")), "Enable?", true)
	if !strings.Contains(yes.String(), "default: Yes") {
		t.Errorf("default-true prompt missing 'default: Yes' hint, got %q", yes.String())
	}
	no := &bytes.Buffer{}
	_ = ynPrompt(no, bufio.NewScanner(strings.NewReader("\n")), "Enable?", false)
	if !strings.Contains(no.String(), "default: No") {
		t.Errorf("default-false prompt missing 'default: No' hint, got %q", no.String())
	}
}

// ynAsk separates "no" from "nobody answered" (waired-agent#754). Empty
// input is still an answer — somebody pressed Enter — and three
// unparseable answers still fall back to the default, because somebody
// IS there and just is not answering the question. Only an exhausted
// stdin is ynNoAnswer.
func TestYnAsk(t *testing.T) {
	name := map[ynAnswer]string{ynYes: "ynYes", ynNo: "ynNo", ynNoAnswer: "ynNoAnswer"}
	cases := []struct {
		in   string
		def  bool
		want ynAnswer
	}{
		{"y\n", true, ynYes},
		{"yes\n", false, ynYes},
		{"Y\n", false, ynYes},
		{"n\n", true, ynNo},
		{"no\n", true, ynNo},
		{"  N  \n", true, ynNo},
		{"\n", true, ynYes},    // Enter on a default-Yes prompt
		{"\n", false, ynNo},    // Enter on a default-No prompt
		{"", true, ynNoAnswer}, // stdin ended before an answer
		{"", false, ynNoAnswer},
		{"maybe\nwhat\nhuh\n", true, ynYes}, // three bad answers → default
		{"maybe\nwhat\nhuh\n", false, ynNo},
	}
	for _, c := range cases {
		var out strings.Builder
		got := ynAsk(&out, bufio.NewScanner(strings.NewReader(c.in)), "Enable?", c.def)
		if got != c.want {
			t.Errorf("ynAsk(%q, def=%v) = %s, want %s", c.in, c.def, name[got], name[c.want])
		}
	}
}

// ynPrompt keeps mapping an exhausted stdin back to the default. Eleven
// call sites outside the benchmark flow read it, and for a configuration
// question an unattended install taking the documented default is the
// designed behaviour — `'0' | waired init …` in
// scripts/dev/installtest-windows.ps1 depends on the prompts after its
// one answer behaving exactly as they did.
func TestYnPrompt_ExhaustedStdinStillTakesTheDefault(t *testing.T) {
	var out strings.Builder
	if got := ynPrompt(&out, bufio.NewScanner(strings.NewReader("")), "Enable?", true); !got {
		t.Errorf("ynPrompt(EOF, def=true) = false, want true")
	}
	if got := ynPrompt(&out, bufio.NewScanner(strings.NewReader("")), "Enable?", false); got {
		t.Errorf("ynPrompt(EOF, def=false) = true, want false")
	}
}
