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

// fakeClaudeRoutingCtl is the test double for management.ClaudeRoutingControl.
type fakeClaudeRoutingCtl struct {
	policy    state.ClaudeRoutingPolicy
	setErr    error
	setCalls  []setClassCall
	stateCall int
}

type setClassCall struct {
	class string
	route state.ClaudeRouteClass
}

func (f *fakeClaudeRoutingCtl) SetClass(_ context.Context, class string, route state.ClaudeRouteClass) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, setClassCall{class, route})
	switch class {
	case state.ClaudeClassMain:
		f.policy.Main = route
	case state.ClaudeClassSub:
		f.policy.Sub = route
	}
	return nil
}

func (f *fakeClaudeRoutingCtl) State() ClaudeRoutingState {
	f.stateCall++
	return ClaudeRoutingState{Policy: f.policy}
}

func newClaudeRoutingTestServer(ctl ClaudeRoutingControl) *Server {
	return New(stubStatus{}, stubPinger{}).WithClaudeRouting(ctl)
}

func doClaudeRoute(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/waired/v1/integration/claude/route", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestClaudeRouting_NotConfigured404(t *testing.T) {
	s := New(stubStatus{}, stubPinger{}) // no WithClaudeRouting
	w := doClaudeRoute(t, s, http.MethodGet, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 when routing control absent", w.Code)
	}
}

func TestClaudeRouting_GetReturnsState(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAnthropic, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	var got ClaudeRoutingState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Policy.Main != state.ClaudeRouteAnthropic || got.Policy.Sub != state.ClaudeRouteSame {
		t.Errorf("state mismatch: %#v", got.Policy)
	}
}

func TestClaudeRouting_SetMainOnly(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"main":"anthropic"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ctl.setCalls) != 1 || ctl.setCalls[0] != (setClassCall{state.ClaudeClassMain, state.ClaudeRouteAnthropic}) {
		t.Errorf("SetClass calls = %#v", ctl.setCalls)
	}
}

func TestClaudeRouting_SetSubOnly(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"sub":"waired"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ctl.setCalls) != 1 || ctl.setCalls[0] != (setClassCall{state.ClaudeClassSub, state.ClaudeRouteWaired}) {
		t.Errorf("SetClass calls = %#v", ctl.setCalls)
	}
}

func TestClaudeRouting_SetBoth(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"main":"anthropic","sub":"waired"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ctl.setCalls) != 2 {
		t.Fatalf("expected 2 SetClass calls, got %#v", ctl.setCalls)
	}
}

func TestClaudeRouting_SubAcceptsSame(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteWaired}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"sub":"same"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if len(ctl.setCalls) != 1 || ctl.setCalls[0].route != state.ClaudeRouteSame {
		t.Errorf("sub=same not dispatched: %#v", ctl.setCalls)
	}
}

func TestClaudeRouting_RejectsSameForMain(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"main":"same"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for main=same", w.Code)
	}
	if len(ctl.setCalls) != 0 {
		t.Error("invalid main must not dispatch SetClass")
	}
}

func TestClaudeRouting_RejectsUnknownRoute(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{"main":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 for unknown route", w.Code)
	}
	if len(ctl.setCalls) != 0 {
		t.Error("unknown route must not dispatch SetClass")
	}
}

func TestClaudeRouting_RejectsEmptyBody(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodPost, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 when neither field provided", w.Code)
	}
}

func TestClaudeRouting_MethodNotAllowed(t *testing.T) {
	ctl := &fakeClaudeRoutingCtl{policy: state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame}}
	s := newClaudeRoutingTestServer(ctl)
	w := doClaudeRoute(t, s, http.MethodDelete, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}
