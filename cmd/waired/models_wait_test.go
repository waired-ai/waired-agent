package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// downloadingThen returns a scripted status sequence: the model
// downloading, then in whichever lane the cancel/finish left it.
func downloadingThen(modelID string, final management.ModelsSnapshot) []management.InferenceStatus {
	return []management.InferenceStatus{
		{Models: management.ModelsSnapshot{Downloading: []string{modelID}}},
		{Models: final},
	}
}

// PRODUCT CONTRACT (waired-agent#794): a `models pull` that watched a
// download start and then saw it disappear stops, and says the download
// stopped.
//
// `models cancel` deletes the job's row server-side, and a deleted row
// is reported as not_present — the same lane a model nobody has ever
// touched sits in. The waiter used to read that as "not started yet" and
// keep polling: the reported host was still waiting five minutes later
// when the harness killed it, and left to itself it would have run the
// full 30-minute deadline and then blamed a timeout for something the
// operator had cancelled on purpose.
func TestWaitForModelReady_CancelledDownloadStopsTheWait(t *testing.T) {
	stub := &pullStub{seq: downloadingThen("qwen3.5-4b",
		management.ModelsSnapshot{NotPresent: []string{"qwen3.5-4b"}})}
	srv := stub.server()
	defer srv.Close()

	out := captureStdout(t, func() {
		err := waitForModelReady(srv.URL, "qwen3.5-4b", 30*time.Second)
		if !errors.Is(err, errModelPullStopped) {
			t.Errorf("err = %v, want errModelPullStopped", err)
		}
	})
	if !strings.Contains(out, "download stopped before it finished") {
		t.Errorf("the wait did not say what happened:\n%s", out)
	}
	// Non-zero for scripts, and silent: the line above is the account a
	// person reads, so main must not print a second one under it.
	code, printErr := exitPlanFor(errModelPullStopped)
	if code != 1 || printErr {
		t.Errorf("exitPlanFor = (%d, %v), want (1, false)", code, printErr)
	}
}

// PRODUCT CONTRACT (waired-agent#794): not_present is only terminal
// AFTER the download has been seen running. A pull that has been
// admitted but has not reached the downloading lane yet reports
// "not started yet" and keeps waiting — that is waired-agent#403, and
// collapsing it into the cancel case would make every slow admission
// look like a cancellation.
func TestWaitForModelReady_NotPresentBeforeAnyDownloadKeepsWaiting(t *testing.T) {
	stub := &pullStub{seq: []management.InferenceStatus{
		{Models: management.ModelsSnapshot{NotPresent: []string{"qwen3.5-4b"}}},
		{Models: management.ModelsSnapshot{NotPresent: []string{"qwen3.5-4b"}}},
		{Models: management.ModelsSnapshot{Ready: []string{"qwen3.5-4b"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := waitForModelReady(srv.URL, "qwen3.5-4b", 30*time.Second); err != nil {
			t.Errorf("err = %v, want nil — the pull finished", err)
		}
	})
	if strings.Contains(out, "download stopped") {
		t.Errorf("a pull that had not started yet was reported as stopped:\n%s", out)
	}
	if !strings.Contains(out, "qwen3.5-4b: ready") {
		t.Errorf("the finished pull was not reported:\n%s", out)
	}
}

// A download that completes normally is unaffected: ready is terminal
// and reports no error.
func TestWaitForModelReady_CompletedDownloadIsNotAStop(t *testing.T) {
	stub := &pullStub{seq: downloadingThen("qwen3.5-4b",
		management.ModelsSnapshot{Ready: []string{"qwen3.5-4b"}})}
	srv := stub.server()
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := waitForModelReady(srv.URL, "qwen3.5-4b", 30*time.Second); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	if strings.Contains(out, "download stopped") {
		t.Errorf("a completed download was reported as stopped:\n%s", out)
	}
}

// A download that FAILS keeps its own ending, with the daemon's reason:
// #328 put that reason on both the line and the error, and the stop
// path must not swallow it.
func TestWaitForModelReady_FailedDownloadKeepsItsReason(t *testing.T) {
	stub := &pullStub{seq: downloadingThen("qwen3.5-4b", management.ModelsSnapshot{
		Failed: []string{"qwen3.5-4b"},
		Failures: []management.ModelFailure{
			{Model: "qwen3.5-4b", Error: "no space left on device"},
		},
	})}
	srv := stub.server()
	defer srv.Close()

	out := captureStdout(t, func() {
		err := waitForModelReady(srv.URL, "qwen3.5-4b", 30*time.Second)
		if err == nil || !strings.Contains(err.Error(), "no space left on device") {
			t.Errorf("err = %v, want the daemon's reason", err)
		}
	})
	if !strings.Contains(out, "no space left on device") {
		t.Errorf("the failure reason is not on the line:\n%s", out)
	}
}

// PRODUCT CONTRACT: modelLane reports the lane, not the rendered line.
// The printed text is product copy and may be reworded; a control-flow
// branch keyed on it would stop firing without any test noticing.
func TestModelLane_ReportsTheLaneNotTheCopy(t *testing.T) {
	body := []byte(`{"models":{"ready":["a"],"downloading":["b"],"failed":["c"],"not_present":["d"]}}`)
	for id, want := range map[string]string{
		"a": laneReady, "b": laneDownloading, "c": laneFailed, "d": laneNotPresent,
		"nosuch": "",
	} {
		if got := modelLane(body, id); got != want {
			t.Errorf("modelLane(%q) = %q, want %q", id, got, want)
		}
	}
	if got := modelLane([]byte("not json"), "a"); got != "" {
		t.Errorf("modelLane on undecodable body = %q, want empty", got)
	}
}
