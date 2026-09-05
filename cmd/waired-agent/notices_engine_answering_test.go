package main

import "testing"

// waired-agent#1220: the notice is the DISAGREEMENT between this
// computer's two observers of its own engine, and nothing else.
//
// PIN: record of today's behaviour for the table; the reason the two
// readings are both required is in engineNotAnswering's own comment.
func TestEngineNotAnswering(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		live, latchedReady, known bool
		want                      bool
	}{
		{"running and not answering is the whole point", false, true, true, true},
		{"answering is the ordinary case", true, true, true, false},
		// The operator turned it off, stopped it, or it failed and said so.
		// Every surface already names that, with its own last_error.
		{"an engine that is not supposed to be up is not a fault", false, false, true, false},
		{"an engine that is off and somehow answering is not this notice", true, false, true, false},
		// Boot, a host with no engine, a probe loop that has not run.
		{"not observed is not a fault", false, true, false, false},
		{"nothing observed at all", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineNotAnswering(tc.live, tc.latchedReady, tc.known); got != tc.want {
				t.Errorf("engineNotAnswering(live=%v, latchedReady=%v, known=%v) = %v, want %v",
					tc.live, tc.latchedReady, tc.known, got, tc.want)
			}
		})
	}
}

// The producer emits it alongside the other engine facts, not instead of
// them — the list rule waired-agent#1229 established.
func TestEngineNotices_NotAnsweringJoinsTheOthers(t *testing.T) {
	got := engineNotices(engineProvenance{
		Engine:         "vllm",
		VersionWarning: "engine version 0.27.0 does not match the bundled pin 0.28.0",
	}, false, true, true)
	if len(got) != 2 {
		t.Fatalf("engineNotices = %+v, want the version warning AND the not-answering notice", got)
	}
}

// The surfaces a person reads must not contradict the notice. On real
// hardware `waired doctor` printed "OK inference engine — ready" and
// "WARN vllm is running but not answering" in the same output, three
// lines apart, because the row reads the adapter's latched state and the
// notice reads the live one.
func TestObservedEngineReadyAccessor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ready     bool
		model     string
		answering func() (bool, bool, bool)
		wantReady bool
		wantModel string
	}{
		{"a running engine that stopped answering is not ready", true, "gpt-oss-20b",
			func() (bool, bool, bool) { return false, true, true }, false, "gpt-oss-20b"},
		{"an answering engine is ready", true, "gpt-oss-20b",
			func() (bool, bool, bool) { return true, true, true }, true, "gpt-oss-20b"},
		// Not observed is not a fault: boot, no engine, a probe loop that
		// has not run.
		{"nothing observed leaves the latch alone", true, "gpt-oss-20b",
			func() (bool, bool, bool) { return false, true, false }, true, "gpt-oss-20b"},
		{"no accessor wired at all", true, "gpt-oss-20b", nil, true, "gpt-oss-20b"},
		// An engine that is already not ready keeps its own answer and its
		// reason; nothing here can make it more not-ready.
		{"a not-ready engine is untouched", false, "gpt-oss-20b",
			func() (bool, bool, bool) { return false, true, true }, false, "gpt-oss-20b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := func() (bool, string) { return tc.ready, tc.model }
			fn := observedEngineReady(base, tc.answering)
			gotReady, gotModel := fn()
			if gotReady != tc.wantReady || gotModel != tc.wantModel {
				t.Errorf("= (%v, %q), want (%v, %q)", gotReady, gotModel, tc.wantReady, tc.wantModel)
			}
		})
	}
}
