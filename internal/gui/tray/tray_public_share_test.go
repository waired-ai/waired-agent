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
	shareDesired    string // "public" | "not_public"
	consentMismatch bool   // when true every consent POST 409s

	warningGets  int
	consentPosts int
	consentSeen  []int
	enablePosts  int
	disablePosts int
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
		case "/waired/v1/public/share":
			writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{
				State: f.shareDesired, DesiredState: f.shareDesired, CPSynced: boolPtr(true),
			})
		case "/waired/v1/public/share/enable":
			f.enablePosts++
			writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{
				State: "public", DesiredState: "public", CPSynced: boolPtr(true),
			})
		case "/waired/v1/public/share/disable":
			f.disablePosts++
			writeJSONResp(w, http.StatusOK, management.PublicShareStateResponse{
				State: "not_public", DesiredState: "not_public", CPSynced: boolPtr(true),
				Note: management.PublicShareDisableNote,
			})
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
	f := &publicFake{warningVersion: 7, shareDesired: "public"}
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
	f := &publicFake{warningVersion: 3, shareDesired: "public"}
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
	f := &publicFake{warningVersion: 3, shareDesired: "public"}
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
	f := &publicFake{warningVersion: 7, shareDesired: "public", consentMismatch: true}
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

func TestRunPublicConsent_EnablesOwnShareWhenOff(t *testing.T) {
	t.Run("share off → reciprocity enables it", func(t *testing.T) {
		f := &publicFake{warningVersion: 1, shareDesired: "not_public"}
		cli := f.start(t)
		(&labelStub{confirmed: true, ok: true}).install(t)

		tr := &tray{cli: cli}
		if !tr.runPublicConsent(context.Background()) {
			t.Fatal("runPublicConsent returned false, want true")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.enablePosts != 1 {
			t.Errorf("enablePosts=%d, want 1 — accepting consent should enable own sharing when off", f.enablePosts)
		}
	})
	t.Run("share already on → no enable", func(t *testing.T) {
		f := &publicFake{warningVersion: 1, shareDesired: "public"}
		cli := f.start(t)
		(&labelStub{confirmed: true, ok: true}).install(t)

		tr := &tray{cli: cli}
		if !tr.runPublicConsent(context.Background()) {
			t.Fatal("runPublicConsent returned false, want true")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.enablePosts != 0 {
			t.Errorf("enablePosts=%d, want 0 — own sharing already on", f.enablePosts)
		}
	})
}

func TestOnPublicShareToggle_DisableRequiresConfirm(t *testing.T) {
	f := &publicFake{shareDesired: "public"}
	cli := f.start(t)
	(&labelStub{confirmed: false, ok: true}).install(t) // user clicks Cancel

	tr := &tray{cli: cli}
	tr.last.PublicShareToggleAction = "Stop sharing this computer publicly"
	tr.onPublicShareToggle(context.Background())

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disablePosts != 0 {
		t.Errorf("disablePosts=%d, want 0 — a cancelled kill-switch confirm must not disable", f.disablePosts)
	}
}

func TestOnPublicShareToggle_DisableUsesServedConfirmCopy(t *testing.T) {
	f := &publicFake{shareDesired: "public"}
	cli := f.start(t)
	stub := &labelStub{confirmed: false, ok: true}
	stub.install(t)

	tr := &tray{cli: cli}
	tr.last.PublicShareToggleAction = "Stop sharing this computer publicly"
	tr.onPublicShareToggle(context.Background())

	if stub.calls != 1 {
		t.Fatalf("confirm dialog calls=%d, want 1", stub.calls)
	}
	if stub.gotTitle != management.PublicShareDisableConfirmTitle {
		t.Errorf("confirm title=%q, want the served constant %q", stub.gotTitle, management.PublicShareDisableConfirmTitle)
	}
	if stub.gotBody != management.PublicShareDisableConfirmText {
		t.Errorf("confirm body=%q, want the served constant %q", stub.gotBody, management.PublicShareDisableConfirmText)
	}
}
