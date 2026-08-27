package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPrintLongContextBench_NamesAnOutOfMemory pins the other end of
// waired-agent#1058: the depth sweep's most informative outcome used to
// print as "measurement failed", which is what every stage that did not
// produce a rate printed.
//
// A record of today's wording. The distinction it protects is the point:
// a transport blip and a GPU that ran out of memory are different facts,
// and only one of them tells a person to look at their window.
func TestPrintLongContextBench_NamesAnOutOfMemory(t *testing.T) {
	var st map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"long_context": {
			"context_length": 200704,
			"completed": false,
			"stages": [
				{"target_tokens": 65536, "prompt_tokens": 64000, "prefill_tok_s": 744, "decode_tok_s": 21.5},
				{"target_tokens": 131072, "failed": true},
				{"target_tokens": 198656, "failed": true, "out_of_memory": true}
			]
		}}`), &st); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got := captureStdout(t, func() { printLongContextBench(st) })

	for _, want := range []string{
		"long-context: @ window 196k (partial)",
		" 64k: prefill 744 tok/s, decode 21.5 tok/s",
		"128k: measurement failed",
		"194k: this computer's GPU ran out of memory",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The out-of-memory stage must not ALSO print the generic line.
	if strings.Count(got, "measurement failed") != 1 {
		t.Errorf("an out-of-memory stage printed the generic failure line too:\n%s", got)
	}
}
