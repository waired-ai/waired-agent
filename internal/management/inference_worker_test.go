package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// fakeWorkerCtl is the test double for management.WorkerController.
// It records the most recent intent so tests can assert dispatch
// happened correctly without spinning up a full agent.
type fakeWorkerCtl struct {
	current state.RoutingPreference
	desired state.RoutingPreference

	lastSetMode state.RoutingMode
	lastSetPin  string
	clearCalls  int
}

func (f *fakeWorkerCtl) SetMode(_ context.Context, mode state.RoutingMode) error {
	f.lastSetMode = mode
	f.current = state.RoutingPreference{Mode: mode}
	f.desired = f.current
	return nil
}

func (f *fakeWorkerCtl) SetPin(_ context.Context, peer string) error {
	f.lastSetPin = peer
	f.current = state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: peer}
	f.desired = f.current
	return nil
}

func (f *fakeWorkerCtl) Clear(_ context.Context) error {
	f.clearCalls++
	f.current = state.RoutingPreference{Mode: state.RoutingModeAuto}
	f.desired = f.current
	return nil
}

func (f *fakeWorkerCtl) State() (current, desired state.RoutingPreference) {
	return f.current, f.desired
}

func newWorkerTestServer(t *testing.T, ctl WorkerController) *Server {
	t.Helper()
	return New(stubStatus{}, stubPinger{}).WithWorkerControl(ctl)
}

func doWorker(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != "" {
		bodyReader = bytes.NewBufferString(body)
	} else {
		bodyReader = &bytes.Buffer{}
	}
	r := httptest.NewRequest(method, "/waired/v1/worker", bodyReader)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestWorkerHandler_GetReturnsState(t *testing.T) {
	ctl := &fakeWorkerCtl{
		current: state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
		desired: state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
	}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var got WorkerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != state.RoutingModePinned || got.PinnedPeerDeviceID != "dev_abc" {
		t.Errorf("response: %+v", got)
	}
	// No InferenceMeshProvider wired → status reports absent.
	if got.PinnedPeerStatus != "absent" {
		t.Errorf("PinnedPeerStatus = %q, want absent (no mesh provider)", got.PinnedPeerStatus)
	}
}

func TestWorkerHandler_PostSetModeAuto(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"auto"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ctl.lastSetMode != state.RoutingModeAuto {
		t.Errorf("lastSetMode = %q, want auto", ctl.lastSetMode)
	}
}

func TestWorkerHandler_PostSetModeLocalOnly(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"local-only"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ctl.lastSetMode != state.RoutingModeLocalOnly {
		t.Errorf("lastSetMode = %q, want local-only", ctl.lastSetMode)
	}
}

func TestWorkerHandler_PostSetPin(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"pinned","pinned_peer_device_id":"dev_xyz"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ctl.lastSetPin != "dev_xyz" {
		t.Errorf("lastSetPin = %q, want dev_xyz", ctl.lastSetPin)
	}
}

func TestWorkerHandler_PostPinnedWithoutPeerRejected(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"pinned"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for pinned without peer, got %d", w.Code)
	}
}

func TestWorkerHandler_PostAutoWithStrayPeerRejected(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"auto","pinned_peer_device_id":"stray"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for auto with stray pin, got %d", w.Code)
	}
}

func TestWorkerHandler_PostUnknownModeRejected(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for unknown mode, got %d", w.Code)
	}
}

func TestWorkerHandler_404WhenNoController(t *testing.T) {
	// No WithWorkerControl → route is not registered, so the loopback
	// mux returns 404 (the standard net/http response for an unknown
	// path).
	s := New(stubStatus{}, stubPinger{})
	w := doWorker(t, s, http.MethodGet, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 without WorkerController, got %d", w.Code)
	}
}

func TestWorkerHandler_MethodNotAllowed(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	// PUT is neither GET nor POST — handler must reject.
	r := httptest.NewRequest(http.MethodPut, "/waired/v1/worker", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", w.Code)
	}
}

func TestWorkerHandler_LoopbackOnly(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	r := httptest.NewRequest(http.MethodGet, "/waired/v1/worker", nil)
	r.RemoteAddr = "10.0.0.5:5555" // non-loopback
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403 for non-loopback, got %d", w.Code)
	}
}
