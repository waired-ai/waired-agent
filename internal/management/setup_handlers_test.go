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
