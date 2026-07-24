package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeLogCtl struct {
	level    string
	setCalls []string
	// invalid, when set, is the level string treated as malformed.
	invalid string
}

func (f *fakeLogCtl) LogLevel(context.Context) (string, error) {
	if f.level == "" {
		return "info", nil
	}
	return f.level, nil
}

func (f *fakeLogCtl) SetLogLevel(_ context.Context, level string) (string, error) {
	f.setCalls = append(f.setCalls, level)
	if level == f.invalid {
		return "", fmt.Errorf("%w: %q", ErrInvalidLogLevel, level)
	}
	f.level = level
	return level, nil
}

func TestLogLevelReturnsCurrent(t *testing.T) {
	lc := &fakeLogCtl{level: "debug"}
	srv := New(fakeStatus{}, fakePinger{}).WithLogController(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/log/level", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("level: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got LogLevelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Level != "debug" {
		t.Fatalf("Level=%q, want debug", got.Level)
	}
}

func TestLogSettingsAppliesLevel(t *testing.T) {
	lc := &fakeLogCtl{level: "info"}
	srv := New(fakeStatus{}, fakePinger{}).WithLogController(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/log/settings",
		strings.NewReader(`{"level":"debug"}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got LogLevelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Level != "debug" {
		t.Fatalf("response Level=%q, want debug", got.Level)
	}
	if len(lc.setCalls) != 1 || lc.setCalls[0] != "debug" {
		t.Fatalf("SetLogLevel not plumbed: calls=%v", lc.setCalls)
	}
}

func TestLogSettingsInvalidLevelIs400(t *testing.T) {
	lc := &fakeLogCtl{level: "info", invalid: "loud"}
	srv := New(fakeStatus{}, fakePinger{}).WithLogController(lc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/log/settings",
		strings.NewReader(`{"level":"loud"}`))
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid level, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogLevelRejectsPOST(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithLogController(&fakeLogCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/log/level", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLogSettingsRejectsGET(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithLogController(&fakeLogCtl{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/log/settings", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLogEndpointsMissingControllerAre404(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithLogController
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/waired/v1/log/level"},
		{http.MethodPost, "/waired/v1/log/settings"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404 without controller, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
