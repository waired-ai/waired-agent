package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEngineSetupAdvice covers both privilege states on all three OSes,
// because what a person is told here depends on both and neither is a
// property of the machine running the tests.
//
// PRODUCT CONTRACT (owner ruling 2026-09-03, waired-agent#1173): a host
// with no engine is pointed at `waired init`, which is the only thing
// that installs one (#138). It is never told that Waired "starts
// fetching them now", which nothing on this path does.
func TestEngineSetupAdvice(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos+"/elevated", func(t *testing.T) {
			got := engineSetupAdvice(goos, true)
			if !strings.Contains(got, "`waired init`") {
				t.Errorf("advice must name the command that installs an engine: %q", got)
			}
			if strings.Contains(got, "sudo") || strings.Contains(got, "Administrator") {
				t.Errorf("an elevated shell must not be told to elevate: %q", got)
			}
		})
		t.Run(goos+"/unelevated", func(t *testing.T) {
			got := engineSetupAdvice(goos, false)
			if !strings.Contains(got, "waired init") {
				t.Errorf("advice must name the command that installs an engine: %q", got)
			}
			want := "sudo"
			if goos == "windows" {
				want = "Administrator"
			}
			if !strings.Contains(got, want) {
				t.Errorf("advice for %s must say how to take privilege; got %q", goos, got)
			}
		})
	}

	// Nothing on this path fetches an engine, so nothing may say it does.
	for _, elevated := range []bool{true, false} {
		if got := engineSetupAdvice("linux", elevated); strings.Contains(got, "fetching") {
			t.Errorf("the promise waired-agent#1173 reported is back: %q", got)
		}
	}
}

func TestElevationCommandFor(t *testing.T) {
	for _, tc := range []struct {
		goos     string
		elevated bool
		want     string
	}{
		{"linux", false, "sudo waired init"},
		{"darwin", false, "sudo waired init"},
		{"windows", false, "waired init"},
		{"linux", true, "waired init"},
		{"windows", true, "waired init"},
	} {
		if got := elevationCommandFor(tc.goos, tc.elevated, "waired init"); got != tc.want {
			t.Errorf("elevationCommandFor(%q, %v) = %q, want %q", tc.goos, tc.elevated, got, tc.want)
		}
	}
}

// TestEngineless: the toggle reads the same status endpoint `waired
// inference status` renders, and a daemon that does not answer is not
// engineless — the ordinary message is the safe one when we do not know.
func TestEngineless(t *testing.T) {
	serve := func(t *testing.T, body any, status int) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	if !engineless(serve(t, map[string]string{"subsystem_state": "no_engine"}, http.StatusOK)) {
		t.Error("no_engine must read as engineless")
	}
	if engineless(serve(t, map[string]string{"subsystem_state": "ready"}, http.StatusOK)) {
		t.Error("a ready engine is not engineless")
	}
	if engineless(serve(t, map[string]string{}, http.StatusOK)) {
		t.Error("a daemon that says nothing must not read as engineless")
	}
	if engineless("http://127.0.0.1:1") {
		t.Error("an unreachable daemon must not read as engineless")
	}
}
