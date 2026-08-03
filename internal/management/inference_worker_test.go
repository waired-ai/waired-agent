package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
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

// waired#1064: the pinned peer's model and the reason it is or is not
// serving are derived HERE, once, so the tray (which reads this body)
// and `waired worker get` cannot disagree about one machine.
func TestWorkerHandler_GetReportsPinnedPeerModelAndCondition(t *testing.T) {
	get := func(peers []inferencemesh.PeerView) WorkerResponse {
		t.Helper()
		ctl := &fakeWorkerCtl{
			current: state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
			desired: state.RoutingPreference{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_abc"},
		}
		s := New(stubStatus{}, stubPinger{}).WithWorkerControl(ctl).
			WithInferenceMesh(&fakeMeshProvider{snapshot: inferencemesh.Snapshot{Peers: peers}})
		w := doWorker(t, s, http.MethodGet, "")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
		}
		var got WorkerResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	serving := get([]inferencemesh.PeerView{{
		DeviceID:   "dev_abc",
		DeviceName: "linux-gpu",
		InferenceState: &signer.InferenceState{
			Reachable: true, Type: signer.InferenceTypeOllama,
			Models:         []string{"qwen3:8b-q4_K_M"},
			ActiveModel:    "qwen3-8b-instruct",
			SubsystemState: signer.SubsystemStateReady,
		},
	}})
	if serving.PinnedPeerStatus != "ok" {
		t.Errorf("status = %q, want ok", serving.PinnedPeerStatus)
	}
	if serving.PinnedPeerModel != "qwen3-8b-instruct" {
		t.Errorf("model = %q, want the catalog id", serving.PinnedPeerModel)
	}
	if serving.PinnedPeerCondition != signer.SubsystemStateReady {
		t.Errorf("condition = %q", serving.PinnedPeerCondition)
	}

	// Mid-download: the engine tag is withdrawn, so the coarse status is
	// still "unavailable" — and the condition is what says why.
	pulling := get([]inferencemesh.PeerView{{
		DeviceID:   "dev_abc",
		DeviceName: "linux-gpu",
		InferenceState: &signer.InferenceState{
			Reachable: true, Type: signer.InferenceTypeOllama,
			ActiveModel:    "qwen3-8b-instruct",
			SubsystemState: signer.SubsystemStateLoading,
		},
	}})
	if pulling.PinnedPeerStatus != "unavailable" {
		t.Errorf("status = %q, want unavailable", pulling.PinnedPeerStatus)
	}
	if pulling.PinnedPeerCondition != signer.SubsystemStateLoading {
		t.Errorf("condition = %q, want loading", pulling.PinnedPeerCondition)
	}
	if pulling.PinnedPeerModel != "qwen3-8b-instruct" {
		t.Errorf("model = %q; a downloading peer still names its model", pulling.PinnedPeerModel)
	}

	// An older peer names its engine tag and gives no reason; the body
	// carries exactly what it did before the fields existed.
	older := get([]inferencemesh.PeerView{{
		DeviceID:   "dev_abc",
		DeviceName: "linux-gpu",
		InferenceState: &signer.InferenceState{
			Reachable: true, Type: signer.InferenceTypeOllama,
			Models: []string{"qwen3:8b-q4_K_M"},
		},
	}})
	if older.PinnedPeerModel != "qwen3:8b-q4_K_M" {
		t.Errorf("model = %q, want the engine tag fallback", older.PinnedPeerModel)
	}

	// A peer that dropped out of the snapshot reports neither.
	absent := get(nil)
	if absent.PinnedPeerStatus != "absent" || absent.PinnedPeerModel != "" || absent.PinnedPeerCondition != "" {
		t.Errorf("absent peer: %+v", absent)
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

func TestWorkerHandler_PostSetModePeerOnly(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"peer-only"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ctl.lastSetMode != state.RoutingModePeerOnly {
		t.Errorf("lastSetMode = %q, want peer-only", ctl.lastSetMode)
	}
}

func TestWorkerHandler_PostPeerOnlyWithStrayPeerRejected(t *testing.T) {
	ctl := &fakeWorkerCtl{}
	s := newWorkerTestServer(t, ctl)

	w := doWorker(t, s, http.MethodPost, `{"mode":"peer-only","pinned_peer_device_id":"dev_xyz"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for peer-only + pin, got %d body=%s", w.Code, w.Body.String())
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
