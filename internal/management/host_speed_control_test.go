package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type fakeHostSpeedCtl struct {
	started bool
	calls   atomic.Int32
}

func (f *fakeHostSpeedCtl) Remeasure(context.Context) bool {
	f.calls.Add(1)
	return f.started
}

func postRemeasure(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/host-speed/remeasure", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The route an install-flow re-run calls before it waits for a figure
// (waired-agent#599). It reports which of the two things happened, because
// the two are not the same event: a fresh measurement is minutes of engine
// time, and a decline means the figure already on hand is this install's own.
func TestHostSpeedRemeasure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started bool
	}{
		{"a re-run starts one", true},
		{"a fresh install reuses what it has", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &fakeHostSpeedCtl{started: tc.started}
			srv := New(fakeStatus{}, fakePinger{}).WithHostSpeedControl(ctl)

			rec := postRemeasure(t, srv)
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			var got HostSpeedRemeasureResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if got.Started != tc.started {
				t.Errorf("started = %v, want %v", got.Started, tc.started)
			}
			if n := ctl.calls.Load(); n != 1 {
				t.Errorf("controller calls = %d, want 1", n)
			}
		})
	}
}

// An older daemon has no such route, and `waired init` treats the 404 as
// "measure nothing extra" — the behaviour it had before the route existed.
func TestHostSpeedRemeasureMissingControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithHostSpeedControl
	if rec := postRemeasure(t, srv); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 when no HostSpeedController", rec.Code)
	}
}

// It takes minutes of engine time, so it is never something a GET can start.
func TestHostSpeedRemeasureRejectsGET(t *testing.T) {
	ctl := &fakeHostSpeedCtl{started: true}
	srv := New(fakeStatus{}, fakePinger{}).WithHostSpeedControl(ctl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/host-speed/remeasure", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
	if n := ctl.calls.Load(); n != 0 {
		t.Errorf("controller calls = %d on a rejected GET, want 0", n)
	}
}
