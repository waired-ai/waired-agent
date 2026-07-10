package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeEngineCtl struct {
	mu       sync.Mutex
	power    EnginePowerState
	managed  bool
	stopErr  error
	startErr error
	stops    int
	starts   int
}

func (f *fakeEngineCtl) StopEngine(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.power = EnginePowerStopped
	return nil
}

func (f *fakeEngineCtl) StartEngine(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	f.power = EnginePowerStarting
	return nil
}

func (f *fakeEngineCtl) EngineState() (EnginePowerState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.power, f.managed
}

func postEngine(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEngineControlStopStart(t *testing.T) {
	ec := &fakeEngineCtl{power: EnginePowerRunning, managed: true}
	srv := New(fakeStatus{}, fakePinger{}).WithEngineControl(ec)

	rec := postEngine(t, srv, "/waired/v1/inference/engine/stop")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got EngineStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Power != "stopped" || !got.Managed {
		t.Fatalf("after stop: %+v", got)
	}
	if ec.stops != 1 {
		t.Errorf("StopEngine called %d times, want 1", ec.stops)
	}

	rec = postEngine(t, srv, "/waired/v1/inference/engine/start")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Power != "starting" {
		t.Fatalf("after start: %+v", got)
	}
	if ec.starts != 1 {
		t.Errorf("StartEngine called %d times, want 1", ec.starts)
	}
}

func TestEngineControlBorrowedIs409(t *testing.T) {
	ec := &fakeEngineCtl{power: EnginePowerRunning, managed: false}
	srv := New(fakeStatus{}, fakePinger{}).WithEngineControl(ec)
	rec := postEngine(t, srv, "/waired/v1/inference/engine/stop")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for reuse mode, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ec.stops != 0 {
		t.Errorf("StopEngine should not be called in reuse mode, got %d", ec.stops)
	}
}

func TestEngineControlMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithEngineControl
	rec := postEngine(t, srv, "/waired/v1/inference/engine/stop")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no EngineController, got %d", rec.Code)
	}
}

func TestEngineControlRejectsGET(t *testing.T) {
	ec := &fakeEngineCtl{power: EnginePowerRunning, managed: true}
	srv := New(fakeStatus{}, fakePinger{}).WithEngineControl(ec)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/engine/stop", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestEngineControlPropagatesError(t *testing.T) {
	ec := &fakeEngineCtl{power: EnginePowerRunning, managed: true, stopErr: errors.New("kill failed")}
	srv := New(fakeStatus{}, fakePinger{}).WithEngineControl(ec)
	rec := postEngine(t, srv, "/waired/v1/inference/engine/stop")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kill failed") {
		t.Errorf("expected error body to mention kill failed, got %s", rec.Body.String())
	}
}

func TestEngineControlStatusSurfacesPower(t *testing.T) {
	ec := &fakeEngineCtl{power: EnginePowerStopped, managed: true}
	srv := New(fakeStatus{}, fakePinger{}).
		WithInference(&fakeInference{canned: InferenceStatus{SubsystemState: "stopped"}}).
		WithEngineControl(ec)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got InferenceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EnginePower != "stopped" || !got.EngineManaged {
		t.Fatalf("status engine fields: power=%q managed=%v", got.EnginePower, got.EngineManaged)
	}
}
