package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// closedMgmtAddr is a loopback address with nothing listening, so a POST
// against it fails at the transport layer with connection-refused — the
// same trick the init/benchmark tests use to exercise the offline path.
const closedMgmtAddr = "http://127.0.0.1:1"

// shareResponseJSON marshals a PublicShareStateResponse the way the
// daemon would, so tests drive runPublicShare/runPublicUnshare through
// the real JSON decode path.
func shareResponseJSON(t *testing.T, r management.PublicShareStateResponse) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return b
}

func TestRunPublicShare_PostsEnableWithMaxClients(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body not JSON: %q", raw)
		}
		_, _ = w.Write(shareResponseJSON(t, management.PublicShareStateResponse{State: string(state.PublicShareOn)}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runPublicShare(srv.URL, "", 4, true, &out); err != nil {
		t.Fatalf("runPublicShare: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/waired/v1/public/share/enable" {
		t.Errorf("path = %q", gotPath)
	}
	if v, ok := gotBody["max_clients"]; !ok || int(v.(float64)) != 4 {
		t.Errorf("body = %v, want max_clients=4", gotBody)
	}
}

func TestRunPublicShare_OmitsBodyWhenMaxClientsUnset(t *testing.T) {
	var bodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyLen = len(raw)
		_, _ = w.Write(shareResponseJSON(t, management.PublicShareStateResponse{State: string(state.PublicShareOn)}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runPublicShare(srv.URL, "", 0, false, &out); err != nil {
		t.Fatalf("runPublicShare: %v", err)
	}
	if bodyLen != 0 {
		t.Errorf("body length = %d, want empty body when --max-clients unset", bodyLen)
	}
}

func TestRunPublicShare_RejectsNegativeMaxClients(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runPublicShare(srv.URL, "", -1, true, &out)
	if err == nil || !strings.Contains(err.Error(), ">= 0") {
		t.Fatalf("err = %v, want a >= 0 rejection", err)
	}
	if hit {
		t.Error("server was contacted; negative --max-clients must be rejected client-side")
	}
}

func TestRunPublicShare_PrintsMeshNoteVerbatim(t *testing.T) {
	// A made-up note stands in for the server-authored mesh/pending copy;
	// the CLI must echo it unchanged.
	const note = "ZZ made-up server note: turning on sharing also did a thing."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(shareResponseJSON(t, management.PublicShareStateResponse{
			State: string(state.PublicShareOn),
			Note:  note,
		}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runPublicShare(srv.URL, "", 0, false, &out); err != nil {
		t.Fatalf("runPublicShare: %v", err)
	}
	if !strings.Contains(out.String(), note) {
		t.Errorf("output = %q, want it to contain the note verbatim", out.String())
	}
}

func TestRunPublicUnshare_PostsDisableAndPrintsRevokedGrants(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = w.Write(shareResponseJSON(t, management.PublicShareStateResponse{
			State:         string(state.PublicShareOff),
			RevokedGrants: 3,
			Note:          management.PublicShareDisableNote,
		}))
	}))
	defer srv.Close()

	var out bytes.Buffer
	// assumeYes=true skips the TTY confirmation (confirmTTY reads real
	// stdin, which is non-interactive under `go test`).
	if err := runPublicUnshare(srv.URL, "", true, &out); err != nil {
		t.Fatalf("runPublicUnshare: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/waired/v1/public/share/disable" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(out.String(), "Guest passes cancelled: 3") {
		t.Errorf("output = %q, want revoked-grants count", out.String())
	}
	if !strings.Contains(out.String(), management.PublicShareDisableNote) {
		t.Errorf("output = %q, want disable note verbatim", out.String())
	}
}

func TestRunPublicUnshare_ConfirmDeclinedMakesNoCall(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	var out bytes.Buffer
	// assumeYes=false + non-TTY stdin ⇒ confirmTTY returns false ⇒ abort
	// before any HTTP call. This is the same seam models_rm relies on
	// (TestModelsRm_NonTTYRequiresYes): a scripted caller without --yes
	// declines. An interactive y/N accept path can't be unit-tested
	// without a real char device, so --yes covers the affirmative branch.
	if err := runPublicUnshare(srv.URL, "", false, &out); err != nil {
		t.Fatalf("runPublicUnshare: %v", err)
	}
	if hit {
		t.Error("server was contacted despite a declined confirmation")
	}
	if !strings.Contains(out.String(), "Cancelled.") {
		t.Errorf("output = %q, want Cancelled.", out.String())
	}
}

func TestRunPublicShare_FallsBackToDesiredWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runPublicShare(closedMgmtAddr, dir, 0, false, &out); err != nil {
		t.Fatalf("runPublicShare (offline): %v", err)
	}
	got, err := state.ReadDesiredPublicShare(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPublicShare: %v", err)
	}
	if got != state.PublicShareOn {
		t.Errorf("desired = %q, want %q", got, state.PublicShareOn)
	}
	if !strings.Contains(out.String(), "persisted; will apply on next start") {
		t.Errorf("output = %q, want persisted notice", out.String())
	}
}

func TestRunPublicUnshare_FallsBackToDesiredWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runPublicUnshare(closedMgmtAddr, dir, true, &out); err != nil {
		t.Fatalf("runPublicUnshare (offline): %v", err)
	}
	got, err := state.ReadDesiredPublicShare(dir)
	if err != nil {
		t.Fatalf("ReadDesiredPublicShare: %v", err)
	}
	if got != state.PublicShareOff {
		t.Errorf("desired = %q, want %q", got, state.PublicShareOff)
	}
}

func TestRunPublicShare_WarnsMaxClientsNotPersistedOffline(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runPublicShare(closedMgmtAddr, dir, 5, true, &out); err != nil {
		t.Fatalf("runPublicShare (offline): %v", err)
	}
	if !strings.Contains(out.String(), "--max-clients was not saved") {
		t.Errorf("output = %q, want a warning that the cap was not saved offline", out.String())
	}
}

func TestRunPublicShare_Unsupported404ReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "public share not configured", http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runPublicShare(srv.URL, "", 0, false, &out)
	if !errors.Is(err, errPublicShareUnsupported) {
		t.Fatalf("err = %v, want errPublicShareUnsupported", err)
	}
}
