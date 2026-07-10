package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestRunInferenceShareOff_FallsBackToDesiredShareWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	ln, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	if err := runInference([]string{"share", "off", "--mgmt", "http://" + ln, "--state-dir", dir}); err != nil {
		t.Fatalf("runInference share off: %v", err)
	}
	got, err := state.ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != state.ShareMeshNotShared {
		t.Errorf("desired-share = %q, want %q", got, state.ShareMeshNotShared)
	}
}

func TestRunInferenceShareOn_FallsBackToDesiredShareWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteDesiredShareMesh(dir, state.ShareMeshNotShared); err != nil {
		t.Fatal(err)
	}
	ln, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	if err := runInference([]string{"share", "on", "--mgmt", "http://" + ln, "--state-dir", dir}); err != nil {
		t.Fatalf("runInference share on: %v", err)
	}
	got, _ := state.ReadDesiredShareMesh(dir)
	if got != state.ShareMeshShared {
		t.Errorf("desired-share = %q, want %q", got, state.ShareMeshShared)
	}
}

func TestRunInferenceShare_HitsDaemonWhenReachable(t *testing.T) {
	dir := t.TempDir()
	var calls int
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		lastPath = r.URL.Path
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"not_shared","desired_state":"not_shared"}`))
	}))
	defer srv.Close()

	// Capture stdout so prettyPrint output doesn't pollute.
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	if err := runInference([]string{"share", "off", "--mgmt", srv.URL, "--state-dir", dir}); err != nil {
		t.Fatalf("runInference share off: %v", err)
	}
	w.Close()
	_ = readAll(t, r)

	if calls != 1 {
		t.Errorf("expected 1 daemon call, got %d", calls)
	}
	if lastPath != "/waired/v1/inference/share/disable" {
		t.Errorf("daemon hit path = %q, want /waired/v1/inference/share/disable", lastPath)
	}
}

func TestRunInferenceShareStatus(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "shared",
			body: `{"subsystem_state":"ready","share_with_mesh":"shared"}`,
			want: "Share with mesh: on",
		},
		{
			name: "not_shared",
			body: `{"subsystem_state":"ready","share_with_mesh":"not_shared"}`,
			want: "Share with mesh: off",
		},
		{
			name: "unsupported_daemon",
			body: `{"subsystem_state":"ready"}`,
			want: "Share with mesh: unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			stdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w
			defer func() { os.Stdout = stdout }()

			if err := runInference([]string{"share", "status", "--mgmt", srv.URL}); err != nil {
				t.Fatalf("runInference share status: %v", err)
			}
			w.Close()
			got := string(readAll(t, r))
			if !strings.Contains(got, tc.want) {
				t.Errorf("status output missing %q; got:\n%s", tc.want, got)
			}
		})
	}
}

func TestRunInference_Errors(t *testing.T) {
	// Under cobra, a namespace command with no subverb (e.g. `inference` or
	// `inference share`) prints help and exits 0 — so only genuinely-unknown
	// subcommands are errors here.
	cases := []struct {
		name string
		args []string
	}{
		{"unknown subverb", []string{"share", "what"}},
		{"unknown top sub", []string{"unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Redirect stderr to swallow the usage output.
			stderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stderr = w
			defer func() { os.Stderr = stderr }()
			err := runInference(tc.args)
			w.Close()
			_ = readAll(t, r)
			if err == nil {
				t.Errorf("expected error for args=%v, got nil", tc.args)
			}
		})
	}
}
