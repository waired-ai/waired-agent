package router

import (
	"context"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// recordingRecorder captures every RecordSelection call for assertion.
type recordingRecorder struct {
	mu          sync.Mutex
	calls       []selectionCall
	pinFailures []pinFailureCall
}

type selectionCall struct {
	decision string
	peerID   string
	model    string
}

type pinFailureCall struct {
	peerID string
	model  string
	reason string
}

func (r *recordingRecorder) RecordSelection(decision, peerID, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, selectionCall{decision, peerID, model})
}

func (r *recordingRecorder) RecordPinnedPeerUnreachable(peerID, model, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pinFailures = append(r.pinFailures, pinFailureCall{peerID, model, reason})
}

func (r *recordingRecorder) snapshot() []selectionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]selectionCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// pinFailureSnapshot returns a copy of recorded pin-failure events;
// kept for downstream tests that need to inspect emit fan-out without
// triggering a data race on r.pinFailures.
//
//nolint:unused // exercised by future router emission tests, kept on the helper sibling for symmetry with snapshot()
func (r *recordingRecorder) pinFailureSnapshot() []pinFailureCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pinFailureCall, len(r.pinFailures))
	copy(out, r.pinFailures)
	return out
}

func newRecorderSelector(rec Recorder) *Selector {
	return NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
		Recorder:   rec,
	})
}

func TestSelectK_EmitsRecordSelection_LocalReady(t *testing.T) {
	rec := &recordingRecorder{}
	s := newRecorderSelector(rec)

	cands, err := s.SelectK(context.Background(), Request{Model: "waired/default"}, 1)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 RecordSelection call, got %d", len(calls))
	}
	if calls[0].decision != "local" {
		t.Errorf("decision: got %q want local", calls[0].decision)
	}
	if calls[0].model != "qwen3-8b-instruct" {
		t.Errorf("model: got %q want qwen3-8b-instruct", calls[0].model)
	}
	if calls[0].peerID != "" {
		t.Errorf("peerID for local should be empty, got %q", calls[0].peerID)
	}
}

func TestSelectK_NoEmitOnError(t *testing.T) {
	rec := &recordingRecorder{}
	s := newRecorderSelector(rec)

	_, err := s.SelectK(context.Background(), Request{Model: "unknown-model"}, 1)
	if err == nil {
		t.Fatalf("SelectK should have errored on unknown model")
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("error path should not emit; got %d calls", len(calls))
	}
}

func TestSelectK_NilRecorderIsSafe(t *testing.T) {
	s := newRecorderSelector(nil)
	_, err := s.SelectK(context.Background(), Request{Model: "waired/default"}, 1)
	if err != nil {
		t.Fatalf("nil Recorder should not break SelectK: %v", err)
	}
}

func TestSelectK_FirstCandidateRepresentsDecision(t *testing.T) {
	// SelectK groups by decision class — cands[1:] share cands[0]'s
	// ExecutionMode. The recorder hook only emits cands[0].
	rec := &recordingRecorder{}
	s := newRecorderSelector(rec)
	cands, err := s.SelectK(context.Background(), Request{Model: "waired/default"}, 3)
	if err != nil {
		t.Fatalf("SelectK: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("expected at least one candidate")
	}
	for i, c := range cands {
		if c.ExecutionMode != cands[0].ExecutionMode {
			t.Errorf("cand %d ExecutionMode=%q diverges from cand 0 %q",
				i, c.ExecutionMode, cands[0].ExecutionMode)
		}
	}
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Fatalf("one SelectK should emit exactly one RecordSelection; got %d", len(calls))
	}
}

func TestSelect_WrapperAlsoEmits(t *testing.T) {
	// Select() is a thin SelectK(1) wrapper; the emit should fire
	// through it unchanged.
	rec := &recordingRecorder{}
	s := newRecorderSelector(rec)
	_, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Fatalf("Select should emit one RecordSelection; got %d", len(calls))
	}
}
