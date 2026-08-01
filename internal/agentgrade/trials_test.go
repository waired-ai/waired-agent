package agentgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// answers is a scripted engine: each call to the tool-requiring cases
// returns the next entry, so a test can make a model comply once and
// misformat the next time.
type scriptedEngine struct {
	mu   sync.Mutex
	n    int
	bad  map[int]bool // 0-based index of tool-case answers that misformat
	seen int
}

func (s *scriptedEngine) handler() http.HandlerFunc {
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

		// The greeting case always behaves.
		if strings.Contains(last, "hello") {
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn"}`))
			return
		}

		s.mu.Lock()
		i := s.n
		s.n++
		s.seen++
		bad := s.bad[i]
		s.mu.Unlock()

		if bad {
			// The rc7 shape: a tool call serialised as assistant text.
			_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"{\"name\":\"Read\",\"arguments\":{\"file_path\":\"/etc/hostname\"}}"}],"stop_reason":"end_turn"}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/etc/hostname"}}],"stop_reason":"tool_use"}`))
	}
}

// The correction that motivated trials: the first sweep graded three
// qwen3.5 models as failing and an immediate re-run graded the same
// three as passing. A model that misformats on ONE trial out of three
// must not come out "pass" — a real session makes hundreds of tool
// calls where this makes six.
func TestRun_intermittentFailureGradesFail(t *testing.T) {
	// 3 trials x 2 tool cases = 6 tool answers; break exactly the fourth.
	eng := &scriptedEngine{bad: map[int]bool{3: true}}
	p := probeAgainst(t, eng.handler())
	p.Trials = 3

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradeFail {
		t.Fatalf("grade = %q, want %q — one bad trial in three is still unusable", rep.Grade, GradeFail)
	}
	if rep.Trials != 3 {
		t.Errorf("trials = %d, want 3", rep.Trials)
	}
	// The instability must be NAMED, not smoothed into a bare verdict:
	// an unstable model and a consistently bad one need different
	// decisions.
	if len(rep.Flaky) == 0 {
		t.Error("a case that disagreed across trials must be listed in Flaky")
	}
	for _, r := range rep.Results {
		if r.Verdict.IsFailure() && !strings.Contains(r.Detail, "not reproducible") {
			t.Errorf("a flaky case's detail should say so, got %q", r.Detail)
		}
	}
}

func TestRun_stablePassIsNotFlaky(t *testing.T) {
	eng := &scriptedEngine{bad: map[int]bool{}}
	p := probeAgainst(t, eng.handler())
	p.Trials = 3

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradePass {
		t.Fatalf("grade = %q, want %q; results=%+v", rep.Grade, GradePass, rep.Results)
	}
	if len(rep.Flaky) != 0 {
		t.Errorf("a model that agreed with itself every trial must not be flaky, got %v", rep.Flaky)
	}
	// 3 trials x 2 tool-requiring cases.
	if eng.seen != 6 {
		t.Errorf("tool cases answered %d times, want 6 (3 trials x 2 cases)", eng.seen)
	}
}

func TestRun_stableFailIsNotFlaky(t *testing.T) {
	eng := &scriptedEngine{bad: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true}}
	p := probeAgainst(t, eng.handler())
	p.Trials = 3

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradeFail {
		t.Fatalf("grade = %q, want %q", rep.Grade, GradeFail)
	}
	if len(rep.Flaky) != 0 {
		t.Errorf("a consistently failing model is not flaky, got %v", rep.Flaky)
	}
}

// An engine that stops answering must end the run immediately rather
// than spending the remaining trials re-measuring the same outage.
func TestRun_errorStopsEarly(t *testing.T) {
	var calls int
	var mu sync.Mutex
	p := probeAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"model runner has unexpectedly stopped"}`))
	})
	p.Trials = 3
	p.Timeout = 5 * time.Second

	rep, err := p.Run(context.Background(), "subject")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Grade != GradeUnknown {
		t.Fatalf("grade = %q, want %q", rep.Grade, GradeUnknown)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls > len(Cases) {
		t.Errorf("made %d calls; a dead engine should stop the run within the first trial (<= %d)",
			calls, len(Cases))
	}
}

func TestSeverityOrdering(t *testing.T) {
	// The worst verdict across trials is the one that survives, so the
	// ordering has to put every failure above pass and errors on top —
	// an error is not a measurement and must not be masked by a later
	// passing trial.
	if severity(VerdictError) <= severity(VerdictMalformedToolCall) {
		t.Error("error must outrank every quality verdict")
	}
	for _, v := range []Verdict{
		VerdictMalformedToolCall, VerdictUnknownTool,
		VerdictUnstructuredToolCall, VerdictNoToolCall, VerdictUnpromptedToolCall,
	} {
		if severity(v) <= severity(VerdictPass) {
			t.Errorf("%q must outrank pass", v)
		}
	}
}
