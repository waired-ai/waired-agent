package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
)

type pullHTTPStatus struct{}

func (pullHTTPStatus) Status() management.Status { return management.Status{} }

type pullHTTPPinger struct{}

func (pullHTTPPinger) PingPeer(context.Context, string) (management.PingResult, error) {
	return management.PingResult{}, nil
}

// THE #305a REGRESSION BAR, end to end. PRODUCT CONTRACT: a pull started
// by POST /waired/v1/models/pull outlives the handler.
//
// net/http cancels r.Context() the moment ServeHTTP returns its 202, and
// the handler passes that context straight into PullModel. Every test in
// internal/management drives the mux with httptest.NewRequest, whose
// Context() is never cancelled — which is exactly why this shipped. So
// this one uses a real server, the real handler and the real provider,
// and it lives here rather than in internal/management because a fake
// provider there could only assert what the handler PASSED, never that
// the download survived.
func TestModelsPullOverHTTP_SurvivesHandlerReturn(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.agentCtx = context.Background()

	srv := httptest.NewServer(management.New(pullHTTPStatus{}, pullHTTPPinger{}).WithInference(p).Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/waired/v1/models/pull", "application/json",
		strings.NewReader(`{"model":"dense-mtp"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d (%s), want 202", resp.StatusCode, body)
	}
	// The handler has returned and the response is fully consumed, so the
	// request context is cancelled by now.

	r.awaitStarted(t)
	r.releaseAll()
	p.waitForPulls()

	if err := r.firstCtxErr(t); err != nil {
		t.Fatalf("pull observed ctx error %v; the download must not die with the handler", err)
	}
	if got := modelStateOf(t, p, "dense-mtp").State; got != catalog.ModelStateReady {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateReady)
	}
}
