package agentgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// classScript answers the read-file case with a different shape on each
// trial, so one run produces several verdict classes for one case.
//
// The scripted engine next door only knows "comply" and "misformat",
// which is enough to reach two classes and not enough to reach the case
// this file is about: a class that occurs and is never the worst.
type classScript struct {
	mu      sync.Mutex
	answers []string // one per read-file trial, in order
	n       int
}

const (
	// A structured call the agent can execute.
	answerValidRead = `{"type":"message","role":"assistant","content":` +
		`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/etc/hostname"}}],` +
		`"stop_reason":"tool_use"}`
	// A structured call to an offered tool, missing the one property its
	// schema marks required -> warn_invalid_tool_arguments.
	answerReadNoPath = `{"type":"message","role":"assistant","content":` +
		`[{"type":"tool_use","id":"t1","name":"Read","input":{}}],` +
		`"stop_reason":"tool_use"}`
	// Prose where a tool was required -> fail_no_tool_call.
	answerProse = `{"type":"message","role":"assistant","content":` +
		`[{"type":"text","text":"It probably contains the hostname."}],"stop_reason":"end_turn"}`
)

func (s *classScript) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := ""
		if n := len(req.Messages); n > 0 {
			last = string(req.Messages[n-1].Content)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(last, "hello"):
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant",` +
				`"content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn"}`))
		case strings.Contains(last, "/etc/hostname"):
			s.mu.Lock()
			a := s.answers[s.n%len(s.answers)]
			s.n++
			s.mu.Unlock()
			_, _ = w.Write([]byte(a))
		default:
			// search-then-edit: always compliant, so it contributes no
			// classes of its own to the assertion below.
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":` +
				`[{"type":"tool_use","id":"t2","name":"Grep","input":{"pattern":"quality_tier"}}],` +
				`"stop_reason":"tool_use"}`))
		}
	}
}

// The gap this whole change exists to close: a verdict class that
// OCCURRED but was never the worst leaves no trace in Verdict or in
// FailedTrials.
//
// Four trials of read-file: two clean, one call the agent cannot execute
// (warn_invalid_tool_arguments), one answer in prose (fail_no_tool_call).
// The stored shape before per-class counts was
// {verdict: fail_no_tool_call, trials: 4, failed: 1} — from which the
// warning is indistinguishable from a second clean trial. That is why
// #455 could not decide whether to promote the warning to a failure even
// after a full catalog sweep ran with the check in place: promotion moves
// a trial from the pass column to the failed one, and nothing recorded
// how many trials would move.
func TestRun_countsEveryVerdictClassNotJustTheWorst(t *testing.T) {
	eng := &classScript{answers: []string{
		answerValidRead, answerReadNoPath, answerValidRead, answerProse,
	}}
	p := probeAgainst(t, eng.handler())
	p.Trials = 4

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got Result
	for _, r := range rep.Results {
		if r.Case == "read-file" {
			got = r
		}
	}
	if got.Case == "" {
		t.Fatalf("no read-file result in %+v", rep.Results)
	}

	// The pre-existing fields still say what they always said.
	if got.Verdict != VerdictNoToolCall {
		t.Errorf("verdict = %q, want %q (the worst class must still win)", got.Verdict, VerdictNoToolCall)
	}
	if got.Trials != 4 || got.FailedTrials != 1 {
		t.Errorf("%d of %d failed, want 1 of 4", got.FailedTrials, got.Trials)
	}

	want := map[Verdict]int{
		VerdictPass:                 2,
		VerdictInvalidToolArguments: 1,
		VerdictNoToolCall:           1,
	}
	if len(got.Verdicts) != len(want) {
		t.Fatalf("verdicts = %v, want %v", got.Verdicts, want)
	}
	for v, n := range want {
		if got.Verdicts[v] != n {
			t.Errorf("verdicts[%s] = %d, want %d (whole tally: %v)", v, got.Verdicts[v], n, got.Verdicts)
		}
	}
}

// Trials, FailedTrials and Flaky are DERIVED from the tally, so they
// cannot describe a different run than it does. Before this they were
// three parallel accumulators.
func TestRun_totalsAreDerivedFromTheTally(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answers     []string
		trials      int
		wantFlaky   bool
		wantFailed  int
		wantClasses int
	}{
		{
			name:        "every trial agrees",
			answers:     []string{answerValidRead},
			trials:      3,
			wantFlaky:   false,
			wantFailed:  0,
			wantClasses: 1,
		},
		{
			name:        "two classes, neither a failure",
			answers:     []string{answerValidRead, answerReadNoPath},
			trials:      2,
			wantFlaky:   true,
			wantFailed:  0, // a warning is not a failure — #455, deliberately
			wantClasses: 2,
		},
		{
			name:        "every trial fails the same way",
			answers:     []string{answerProse},
			trials:      3,
			wantFlaky:   false,
			wantFailed:  3,
			wantClasses: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &classScript{answers: tc.answers}
			p := probeAgainst(t, eng.handler())
			p.Trials = tc.trials

			rep, err := p.Run(context.Background(), "subject")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var got Result
			for _, r := range rep.Results {
				if r.Case == "read-file" {
					got = r
				}
			}

			sum := 0
			failed := 0
			for v, n := range got.Verdicts {
				sum += n
				if v.IsFailure() {
					failed += n
				}
			}
			if sum != got.Trials || got.Trials != tc.trials {
				t.Errorf("tally sums to %d, Trials says %d, ran %d", sum, got.Trials, tc.trials)
			}
			if failed != got.FailedTrials || failed != tc.wantFailed {
				t.Errorf("tally has %d failures, FailedTrials says %d, want %d",
					failed, got.FailedTrials, tc.wantFailed)
			}
			if len(got.Verdicts) != tc.wantClasses {
				t.Errorf("classes = %v, want %d of them", got.Verdicts, tc.wantClasses)
			}
			flaky := false
			for _, f := range rep.Flaky {
				if f == "read-file" {
					flaky = true
				}
			}
			if flaky != tc.wantFlaky {
				t.Errorf("flaky = %v, want %v (classes: %v)", flaky, tc.wantFlaky, got.Verdicts)
			}
		})
	}
}

// collect runs mid-run on the engine-error path, so the Result it hands
// back must not keep changing as later trials tally into the accumulator.
func TestTallyCopiesTheAccumulator(t *testing.T) {
	live := map[Verdict]int{VerdictPass: 1}
	got, trials, failed := tally(live)
	live[VerdictPass] = 99
	live[VerdictNoToolCall] = 7

	if got[VerdictPass] != 1 || len(got) != 1 {
		t.Errorf("tally aliased its input: %v", got)
	}
	if trials != 1 || failed != 0 {
		t.Errorf("trials=%d failed=%d, want 1/0", trials, failed)
	}
	if n, _, _ := tally(nil); n != nil {
		t.Errorf("an empty tally must stay nil so it is omitted, got %v", n)
	}
}
