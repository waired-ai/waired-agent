package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSetupExecutor records the requests the handler forwards and echoes
// a scripted state back.
type fakeSetupExecutor struct {
	mu    sync.Mutex
	state SetupStateResponse
	notes []SetupExecutorRequest
}

func (f *fakeSetupExecutor) SetupState(context.Context) SetupStateResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeSetupExecutor) NoteExecutor(_ context.Context, req SetupExecutorRequest) SetupStateResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, req)
	f.state.ExecutorAttached = req.Attached
	f.state.ExecutorElevated = req.Attached && req.Elevated
	return f.state
}

func (f *fakeSetupExecutor) noted() []SetupExecutorRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SetupExecutorRequest(nil), f.notes...)
}

// TestSetupRoutesAbsentWithoutController pins the probe an older-CLI /
// newer-CLI handshake depends on: 404 means "this daemon predates the
// executor lease", so the CLI must be able to rely on the routes simply
// not existing when no controller is attached.
func TestSetupRoutesAbsentWithoutController(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})
	for _, path := range []string{"/waired/v1/setup/state", "/waired/v1/setup/executor"} {
		rec := httptest.NewRecorder()
		srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s without a controller = %d, want 404", path, rec.Code)
		}
	}
}

func TestSetupStateHandler(t *testing.T) {
	f := &fakeSetupExecutor{state: SetupStateResponse{
		Active:          true,
		DesiredEngine:   "ollama",
		DesiredModelID:  "m-1",
		EngineInstalled: true,
		InstallClaimed:  "ollama",
		StateDir:        "/var/lib/waired",
	}}
	srv := New(fakeStatus{}, fakePinger{}).WithSetupExecutor(f)

	rec := httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/waired/v1/setup/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup/state = %d, want 200", rec.Code)
	}
	var got SetupStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Active || got.DesiredEngine != "ollama" || got.InstallClaimed != "ollama" {
		t.Fatalf("state = %+v, want the scripted projection", got)
	}
	// The executor installs relative to this; a dropped field would send
	// the engine somewhere the daemon never looks (waired#835 §11.1).
	if got.StateDir != "/var/lib/waired" {
		t.Fatalf("state = %+v, want the daemon's state dir on the wire", got)
	}

	rec = httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/waired/v1/setup/state", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /setup/state = %d, want 405", rec.Code)
	}
}

func TestSetupExecutorHandlerRoundTrip(t *testing.T) {
	f := &fakeSetupExecutor{}
	srv := New(fakeStatus{}, fakePinger{}).WithSetupExecutor(f)

	body := `{"attached":true,"elevated":true,"phase":"installing","engine":"ollama"}`
	rec := httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/waired/v1/setup/executor", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d, want 200", rec.Code)
	}
	var got SetupStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.ExecutorAttached || !got.ExecutorElevated {
		t.Fatalf("attach response = %+v, want the lease reflected back", got)
	}

	rec = httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/waired/v1/setup/executor", strings.NewReader(`{"attached":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("release = %d, want 200", rec.Code)
	}

	notes := f.noted()
	if len(notes) != 2 || !notes[0].Attached || notes[0].Phase != SetupExecutorPhaseInstalling || notes[1].Attached {
		t.Fatalf("forwarded requests = %+v, want attach(installing) then release", notes)
	}
}

// TestSetupExecutorHandlerRejectsBadInput: an empty body is a valid bare
// attach, but malformed JSON and an unknown phase are not.
func TestSetupExecutorHandlerRejectsBadInput(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithSetupExecutor(&fakeSetupExecutor{})
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty body is a bare attach", "", http.StatusOK},
		{"malformed json", "{", http.StatusBadRequest},
		{"unknown phase", `{"attached":true,"phase":"exploding"}`, http.StatusBadRequest},
		{"known phase", `{"attached":true,"phase":"done"}`, http.StatusOK},
		// A step outside the settled set would open a row in the wizard
		// that nothing ever closes (waired-agent#197).
		{"unknown step", `{"attached":true,"step":"engine_warp"}`, http.StatusBadRequest},
		{"known step", `{"attached":true,"step":"engine_download"}`, http.StatusOK},
		{"absent step is the install", `{"attached":true,"phase":"installing"}`, http.StatusOK},
		// The control plane rejects a negative rate outright, so a report
		// carrying the renderer's "unknown" sentinel must be caught here
		// rather than silently poisoning a push the CP will 400.
		{"negative rate", `{"attached":true,"step":"engine_download","rate_bps":-1}`, http.StatusBadRequest},
		{"negative bytes", `{"attached":true,"step":"engine_download","completed_bytes":-5}`, http.StatusBadRequest},
		// The declared error code (waired-agent#135) is copied onto the
		// step and pushed to a CP that validates the enum, so an unknown
		// one has to be refused here rather than 400-ing every subsequent
		// push from this device.
		{"unknown error code", `{"attached":true,"phase":"failed","error_code":"sad"}`, http.StatusBadRequest},
		{"known error code", `{"attached":true,"phase":"failed","error_code":"permission_denied"}`, http.StatusOK},
		{"absent error code is 'you classify it'", `{"attached":true,"phase":"failed","error":"boom"}`, http.StatusOK},
		// The terminal names what it configured, because a terminal-driven
		// init has no instruction for the daemon to read the names from
		// (waired-agent#646). Unknown and retired ids are dropped further in
		// rather than refused here — a CLI newer or older than the daemon it
		// drives is the ordinary state around an upgrade — but a list this
		// long is a malformed request, and the daemon persists it.
		{
			"integration targets", `{"attached":true,"step":"integration","phase":"done",` +
				`"integration_targets":["claude-code","openclaw"]}`, http.StatusOK,
		},
		{
			"unknown integration target is tolerated", `{"attached":true,"step":"integration","phase":"done",` +
				`"integration_targets":["cursor"]}`, http.StatusOK,
		},
		{
			"too many integration targets", `{"attached":true,"step":"integration","phase":"done",` +
				`"integration_targets":["a","b","c","d","e","f","g","h",` +
				`"i","j","k","l","m","n","o","p","q"]}`, http.StatusBadRequest,
		},
	} {
		rec := httptest.NewRecorder()
		srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/waired/v1/setup/executor", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}

	rec := httptest.NewRecorder()
	srv.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/waired/v1/setup/executor", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /setup/executor = %d, want 405", rec.Code)
	}
}
