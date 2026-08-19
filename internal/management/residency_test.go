package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeResidencyCtl records the real argument (a fake that dropped it
// would make the "wrong value reached the daemon" case unwritable).
type fakeResidencyCtl struct {
	idle     time.Duration
	setCalls []time.Duration
	// invalid, when set, is the value treated as unusable.
	invalid time.Duration
	// getErr, when set, is returned by Residency.
	getErr error
}

func (f *fakeResidencyCtl) Residency(context.Context) (time.Duration, error) {
	if f.getErr != nil {
		return 0, f.getErr
	}
	return f.idle, nil
}

func (f *fakeResidencyCtl) SetResidency(_ context.Context, idle time.Duration) (time.Duration, error) {
	f.setCalls = append(f.setCalls, idle)
	if f.invalid != 0 && idle == f.invalid {
		return 0, fmt.Errorf("%w: %v", ErrInvalidResidency, idle)
	}
	f.idle = idle
	return idle, nil
}

func residencyRequest(t *testing.T, method, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/waired/v1/inference/residency", nil)
	} else {
		r = httptest.NewRequest(method, "/waired/v1/inference/residency", strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:1"
	return r
}

func TestResidencyGetReportsCurrent(t *testing.T) {
	rc := &fakeResidencyCtl{idle: 30 * time.Minute}
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got ResidencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.IdleTimeout != "30m0s" || got.HoldsIndefinitely {
		t.Fatalf("got %+v, want 30m0s and not indefinite", got)
	}
}

// TestResidencyGetSpellsOutIndefinite pins the product decision that a
// zero means "never unload" (owner ruling on waired-agent#861, recorded
// in docs/decisions/20260820/0130-model-residency-is-a-setting.md). A
// client that read the zero as a number would render "unloads in 0s" —
// the opposite — so the boolean travels with it.
func TestResidencyGetSpellsOutIndefinite(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(&fakeResidencyCtl{idle: 0})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodGet, ""))
	var got ResidencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.HoldsIndefinitely {
		t.Fatalf("HoldsIndefinitely=false for a zero idle timeout: %+v", got)
	}
}

func TestResidencyPostAppliesValue(t *testing.T) {
	rc := &fakeResidencyCtl{}
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodPost, `{"idle_timeout":"45m"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(rc.setCalls) != 1 || rc.setCalls[0] != 45*time.Minute {
		t.Fatalf("setCalls=%v, want [45m0s]", rc.setCalls)
	}
	var got ResidencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.IdleTimeout != "45m0s" {
		t.Fatalf("IdleTimeout=%q, want 45m0s", got.IdleTimeout)
	}
}

// TestResidencyPostNegativeIsIndefinite records today's behaviour: a
// negative duration is the engine's own spelling of "never unload", so
// it is normalized rather than rejected.
func TestResidencyPostNegativeIsIndefinite(t *testing.T) {
	rc := &fakeResidencyCtl{}
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodPost, `{"idle_timeout":"-5m"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(rc.setCalls) != 1 || rc.setCalls[0] != 0 {
		t.Fatalf("setCalls=%v, want [0s]", rc.setCalls)
	}
}

func TestResidencyPostRejectsGarbage(t *testing.T) {
	rc := &fakeResidencyCtl{}
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodPost, `{"idle_timeout":"soon"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(rc.setCalls) != 0 {
		t.Fatalf("controller was called with an unparseable value: %v", rc.setCalls)
	}
}

func TestResidencyPostControllerRejection(t *testing.T) {
	rc := &fakeResidencyCtl{invalid: time.Second}
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodPost, `{"idle_timeout":"1s"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestResidencyMethodNotAllowed(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(&fakeResidencyCtl{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, residencyRequest(t, http.MethodDelete, ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d, want 405", rec.Code)
	}
}

// TestResidencyWithoutControllerIs404 is the shape every optional
// controller here uses, and it is what lets a tray or CLI tell "this
// daemon is too old" from "this daemon failed".
func TestResidencyWithoutControllerIs404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{})

	for _, m := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		body := ""
		if m == http.MethodPost {
			body = `{"idle_timeout":"1h"}`
		}
		srv.Handler().ServeHTTP(rec, residencyRequest(t, m, body))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d, want 404", m, rec.Code)
		}
	}
}

// TestParseResidency covers the spellings the surfaces accept. The words
// exist so a client does not have to know the product spells "never" as
// a zero; the duration forms are time.ParseDuration's.
func TestParseResidency(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"never", 0, false},
		{"off", 0, false},
		{"indefinite", 0, false},
		{"keep", 0, false},
		{"-1h", 0, false},
		{"30m", 30 * time.Minute, false},
		{"8h", 8 * time.Hour, false},
		{"45s", 45 * time.Second, false},
		{"soon", 0, true},
		{"10", 0, true},
	} {
		got, err := ParseResidency(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseResidency(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("ParseResidency(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestInferenceStatusCarriesResidency pins the arrangement the tray
// depends on: the current setting rides the status body it already polls
// every 5 s, rather than a second read the menu would have to make.
func TestInferenceStatusCarriesResidency(t *testing.T) {
	srv := newServerWithInference(&fakeInference{canned: InferenceStatus{SubsystemState: "ready"}}).
		WithResidencyControl(&fakeResidencyCtl{idle: time.Hour})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got InferenceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Residency == nil || got.Residency.IdleTimeout != "1h0m0s" {
		t.Fatalf("Residency=%+v, want 1h0m0s", got.Residency)
	}
}
