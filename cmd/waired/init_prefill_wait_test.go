package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// prefillStatusServer serves /inference/status with a stage that the test
// can move. A mux rather than a bare handler: an httptest listener is not
// private to the test that started it, and a handler answering every path
// would answer somebody else's poll too.
func prefillStatusServer(t *testing.T, stage *atomic.Value) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := stage.Load().(string)
		_ = json.NewEncoder(w).Encode(management.InferenceStatus{PrefillMeasurementStage: s})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func withFastPrefillWait(t *testing.T) {
	t.Helper()
	budget, poll, narrate := prefillWaitBudget, prefillWaitPoll, prefillNarrateEvery
	prefillWaitBudget, prefillWaitPoll, prefillNarrateEvery = 2*time.Second, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		prefillWaitBudget, prefillWaitPoll, prefillNarrateEvery = budget, poll, narrate
	})
}

// THE waired-agent#1301 REGRESSION BAR, terminal half. PRODUCT CONTRACT
// (owner, rc6 review; the ruling in
// docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md):
// `waired init` waits for this measurement.
//
// Until now it did not — `grep measuring cmd/waired` found nothing — so
// the completion box printed about four minutes before the work
// finished, with the GPU busy and peers being refused.
func TestWaitPrefillMeasurement_WaitsUntilTerminal(t *testing.T) {
	withFastPrefillWait(t)
	var stage atomic.Value
	stage.Store("measuring")
	url := prefillStatusServer(t, &stage)

	go func() {
		time.Sleep(60 * time.Millisecond)
		stage.Store("measured")
	}()

	var out bytes.Buffer
	start := time.Now()
	waitPrefillMeasurement(url, &out)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("returned after %v, before the measurement finished", elapsed)
	}
	if !strings.Contains(out.String(), "Timing this computer") {
		t.Errorf("the wait said nothing about itself:\n%s", out.String())
	}
}

// A host that has already measured — a re-run of init, or a machine that
// finished during the model wait — must not be announced at all. A line
// about minutes of work in front of a five-millisecond gap is noise, the
// same reasoning init_host_speed.go records for its own announcement.
func TestWaitPrefillMeasurement_SaysNothingWhenAlreadyDone(t *testing.T) {
	withFastPrefillWait(t)
	var stage atomic.Value
	stage.Store("measured")
	url := prefillStatusServer(t, &stage)

	var out bytes.Buffer
	waitPrefillMeasurement(url, &out)
	if out.Len() != 0 {
		t.Errorf("printed for a measurement that was already done:\n%s", out.String())
	}
}

// A daemon predating prefill_measurement_stage reports nothing. A newer
// CLI must not spend its whole budget waiting for a signal that will
// never arrive.
func TestWaitPrefillMeasurement_OlderDaemonIsTerminal(t *testing.T) {
	withFastPrefillWait(t)
	var stage atomic.Value
	stage.Store("")
	url := prefillStatusServer(t, &stage)

	var out bytes.Buffer
	start := time.Now()
	waitPrefillMeasurement(url, &out)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v on a daemon that reports no stage", elapsed)
	}
	if out.Len() != 0 {
		t.Errorf("announced a wait that did not happen:\n%s", out.String())
	}
}

// The budget ends the wait, and says so rather than dropping it: the box
// about to call this computer set up is printed over work that is still
// running.
func TestWaitPrefillMeasurement_BudgetEndsItOutLoud(t *testing.T) {
	withFastPrefillWait(t)
	prefillWaitBudget = 80 * time.Millisecond
	var stage atomic.Value
	stage.Store("measuring")
	url := prefillStatusServer(t, &stage)

	var out bytes.Buffer
	start := time.Now()
	waitPrefillMeasurement(url, &out)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the budget did not end the wait: %v", elapsed)
	}
	if !strings.Contains(out.String(), "carrying on") {
		t.Errorf("gave up silently:\n%s", out.String())
	}
}

// prefillStageTerminal is the CLI's own copy of the daemon's vocabulary;
// this is the table that keeps the two spellings agreeing.
func TestPrefillStageTerminal(t *testing.T) {
	for _, tc := range []struct {
		stage string
		want  bool
	}{
		{"", true}, // a daemon predating the field
		{"measured", true},
		{"failed", true},
		{"gave_up", true},
		{"measuring", false},
		{"something-a-newer-daemon-invents", false},
	} {
		if got := prefillStageTerminal(tc.stage); got != tc.want {
			t.Errorf("prefillStageTerminal(%q) = %v, want %v", tc.stage, got, tc.want)
		}
	}
}
