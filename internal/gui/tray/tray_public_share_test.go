package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// publicFake is a configurable in-memory daemon for the public-share
// handler tests. It counts the writes the tray made so assertions can
// prove, e.g., that a cancelled consent recorded nothing.
type publicFake struct {
	mu sync.Mutex

	warningVersion  int
	consentMismatch bool // when true every consent POST 409s

	warningGets  int
	consentPosts int
	consentSeen  []int
}

func (f *publicFake) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/waired/v1/public/warning":
			f.warningGets++
			writeJSONResp(w, http.StatusOK, management.PublicWarningResponse{
				Version: f.warningVersion, Title: "T", Text: "body", AcceptLabel: "A", CancelLabel: "C",
			})
		case "/waired/v1/public/consent":
			f.consentPosts++
			var req management.PublicConsentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.consentSeen = append(f.consentSeen, req.WarningVersion)
			if f.consentMismatch {
				writeJSONResp(w, http.StatusConflict, map[string]string{"error_code": "warning_version_mismatch"})
				return
			}
			writeJSONResp(w, http.StatusOK, management.PublicUseResponse{Mode: "auto", Consented: true})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL)
}

// labelStub swaps the package confirmWithLabels seam for a deterministic
// stub, recording the copy it was handed. Restored on cleanup.
type labelStub struct {
	confirmed, ok bool
	gotTitle      string
	gotBody       string
	calls         int
}

func (s *labelStub) install(t *testing.T) {
	t.Helper()
	orig := confirmWithLabels
	confirmWithLabels = func(title, body, _, _ string) (bool, bool) {
		s.calls++
		s.gotTitle = title
		s.gotBody = body
		return s.confirmed, s.ok
	}
	t.Cleanup(func() { confirmWithLabels = orig })
}

func TestRunPublicConsent_PostsDisplayedWarningVersion(t *testing.T) {
	f := &publicFake{warningVersion: 7}
	cli := f.start(t)
	(&labelStub{confirmed: true, ok: true}).install(t)

	tr := &tray{cli: cli}
	if !tr.runPublicConsent(context.Background()) {
		t.Fatal("runPublicConsent returned false, want true")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consentPosts != 1 {
		t.Fatalf("consentPosts=%d, want 1", f.consentPosts)
	}
	if len(f.consentSeen) != 1 || f.consentSeen[0] != 7 {
		t.Errorf("consent versions=%v, want [7] (the displayed version)", f.consentSeen)
	}
}

func TestRunPublicConsent_CancelRecordsNothing(t *testing.T) {
	f := &publicFake{warningVersion: 3}
	cli := f.start(t)
	(&labelStub{confirmed: false, ok: true}).install(t)

	tr := &tray{cli: cli}
	if tr.runPublicConsent(context.Background()) {
		t.Error("runPublicConsent returned true on cancel, want false")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consentPosts != 0 {
		t.Errorf("consentPosts=%d, want 0 — a cancelled dialog must record no consent", f.consentPosts)
	}
}

func TestRunPublicConsent_NoDialogBackendRecordsNothing(t *testing.T) {
	f := &publicFake{warningVersion: 3}
	cli := f.start(t)
	(&labelStub{ok: false}).install(t) // no backend

	tr := &tray{cli: cli}
	if tr.runPublicConsent(context.Background()) {
		t.Error("runPublicConsent returned true with no dialog backend, want false")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consentPosts != 0 {
		t.Errorf("consentPosts=%d, want 0 — consent must not be recorded without showing the text", f.consentPosts)
	}
}

func TestRunPublicConsent_VersionMismatchRefetchesOnce(t *testing.T) {
	// Every consent POST 409s: the flow must re-fetch the warning exactly
	// once, retry exactly once, then give up (no unbounded loop).
	f := &publicFake{warningVersion: 7, consentMismatch: true}
	cli := f.start(t)
	(&labelStub{confirmed: true, ok: true}).install(t)

	tr := &tray{cli: cli}
	if tr.runPublicConsent(context.Background()) {
		t.Error("runPublicConsent returned true despite persistent mismatch, want false")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.warningGets != 2 {
		t.Errorf("warningGets=%d, want 2 (initial + exactly one re-fetch)", f.warningGets)
	}
	if f.consentPosts != 2 {
		t.Errorf("consentPosts=%d, want 2 (initial + exactly one retry)", f.consentPosts)
	}
}
