package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// workerTestServer wires a mux that emulates the loopback management
// API for both /waired/v1/worker (POST/GET) and /waired/v1/inference/mesh
// (GET, used by `waired worker set --pin=<name>` peer-name resolution).
func workerTestServer(t *testing.T, snap inferencemesh.Snapshot) (*httptest.Server, *workerSpy) {
	t.Helper()
	spy := &workerSpy{state: management.WorkerResponse{Mode: state.RoutingModeAuto}}
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/worker", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(spy.state)
		case http.MethodPost:
			var req management.WorkerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			spy.posts = append(spy.posts, req)
			spy.state = management.WorkerResponse{
				Mode:               req.Mode,
				PinnedPeerDeviceID: req.PinnedPeerDeviceID,
			}
			_ = json.NewEncoder(w).Encode(spy.state)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/waired/v1/inference/mesh", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(snap)
	})
	return httptest.NewServer(mux), spy
}

type workerSpy struct {
	state management.WorkerResponse
	posts []management.WorkerRequest
}

func TestWorkerGet_RendersMode(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runWorker([]string{"get", "--mgmt", srv.URL}); err != nil {
			t.Fatalf("runWorker get: %v", err)
		}
	})
	if !strings.Contains(out, "mode:") {
		t.Errorf("output should mention mode: %q", out)
	}
}

func TestWorkerSet_ModeAuto(t *testing.T) {
	srv, spy := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()

	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--mode=auto"}); err != nil {
			t.Fatalf("runWorker set: %v", err)
		}
	})
	if got := len(spy.posts); got != 1 {
		t.Fatalf("want 1 POST, got %d", got)
	}
	if spy.posts[0].Mode != state.RoutingModeAuto {
		t.Errorf("POSTed mode = %q, want auto", spy.posts[0].Mode)
	}
}

func TestWorkerSet_ModeLocalOnly(t *testing.T) {
	srv, spy := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--mode=local-only"}); err != nil {
			t.Fatalf("runWorker set: %v", err)
		}
	})
	if spy.posts[0].Mode != state.RoutingModeLocalOnly {
		t.Errorf("mode = %q, want local-only", spy.posts[0].Mode)
	}
}

func TestWorkerSet_PinByDeviceID(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev_abc", DeviceName: "linux-gpu", InferenceState: &signer.InferenceState{Reachable: true}},
		},
	}
	srv, spy := workerTestServer(t, snap)
	defer srv.Close()
	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--pin=dev_abc"}); err != nil {
			t.Fatalf("runWorker set --pin: %v", err)
		}
	})
	if got := spy.posts[0]; got.Mode != state.RoutingModePinned || got.PinnedPeerDeviceID != "dev_abc" {
		t.Errorf("POST = %+v, want pinned+dev_abc", got)
	}
}

func TestWorkerSet_PinByName(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev_xyz", DeviceName: "alice-laptop", InferenceState: &signer.InferenceState{Reachable: true}},
		},
	}
	srv, spy := workerTestServer(t, snap)
	defer srv.Close()
	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--pin=alice-laptop"}); err != nil {
			t.Fatalf("runWorker set: %v", err)
		}
	})
	if spy.posts[0].PinnedPeerDeviceID != "dev_xyz" {
		t.Errorf("name resolution failed: got %q, want dev_xyz", spy.posts[0].PinnedPeerDeviceID)
	}
}

func TestWorkerSet_PinAmbiguousNameRejected(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev_a", DeviceName: "node", InferenceState: &signer.InferenceState{Reachable: true}},
			{DeviceID: "dev_b", DeviceName: "node", InferenceState: &signer.InferenceState{Reachable: true}},
		},
	}
	srv, _ := workerTestServer(t, snap)
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--pin=node"})
	})
	if err == nil {
		t.Fatal("expected error for ambiguous name")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity: %v", err)
	}
}

func TestWorkerSet_PinMissingPeerRejected(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--pin=nope"})
	})
	if err == nil {
		t.Fatal("expected error for missing peer")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not-found: %v", err)
	}
}

func TestWorkerSet_PinnedModeWithoutPinRejected(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--mode=pinned"})
	})
	if err == nil {
		t.Fatal("expected error for --mode=pinned without --pin")
	}
}

func TestWorkerSet_PinWithIncompatibleModeRejected(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{DeviceID: "dev_a", InferenceState: &signer.InferenceState{Reachable: true}}},
	})
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--mode=local-only", "--pin=dev_a"})
	})
	if err == nil {
		t.Fatal("expected error for --pin with --mode=local-only")
	}
}

func TestWorkerSet_NoFlagsRejected(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL})
	})
	if err == nil {
		t.Fatal("expected error when neither --mode nor --pin set")
	}
}

func TestWorkerSet_UnknownModeRejected(t *testing.T) {
	srv, _ := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--mode=bogus"})
	})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestWorkerUnknownSubcommandRejected(t *testing.T) {
	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"oops"})
	})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

// Sanity-check workerURL builds the expected URL from both addr forms
// (host:port and http://host:port) so callers don't accidentally
// double-prefix.
func TestWorkerURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"127.0.0.1:9476", "http://127.0.0.1:9476/waired/v1/worker"},
		{"http://127.0.0.1:9476", "http://127.0.0.1:9476/waired/v1/worker"},
		{"http://127.0.0.1:9476/", "http://127.0.0.1:9476/waired/v1/worker"},
	}
	for _, c := range cases {
		got := workerURL(c.in)
		if got != c.want {
			t.Errorf("workerURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Make sure `worker get --json` returns the raw WorkerResponse — the
// tray prefers JSON, so a downstream wrapper relying on that format
// should not see the human-readable banner.
func TestWorkerGet_JSON(t *testing.T) {
	srv, spy := workerTestServer(t, inferencemesh.Snapshot{})
	spy.state = management.WorkerResponse{Mode: state.RoutingModePinned, PinnedPeerDeviceID: "dev_z"}
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runWorker([]string{"get", "--mgmt", srv.URL, "--json"}); err != nil {
			t.Fatalf("runWorker get --json: %v", err)
		}
	})
	var resp management.WorkerResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output should be JSON: %v (raw=%s)", err, out)
	}
	if resp.Mode != state.RoutingModePinned {
		t.Errorf("decoded mode = %q, want pinned", resp.Mode)
	}
}

// Probe for stable rendering of all 4 modes in non-JSON output.
func TestPrintWorkerResponse_AllModes(t *testing.T) {
	cases := []state.RoutingMode{
		state.RoutingModeAuto, state.RoutingModeLocalOnly, state.RoutingModePeerPreferred,
	}
	for _, m := range cases {
		t.Run(string(m), func(t *testing.T) {
			out := captureStdout(t, func() {
				printWorkerResponse(os.Stdout, management.WorkerResponse{Mode: m})
			})
			if !strings.Contains(out, fmt.Sprintf("mode:        %s", m)) {
				t.Errorf("mode label missing: %q", out)
			}
		})
	}
	t.Run("pinned", func(t *testing.T) {
		out := captureStdout(t, func() {
			printWorkerResponse(os.Stdout, management.WorkerResponse{
				Mode:               state.RoutingModePinned,
				PinnedPeerDeviceID: "dev_abc",
				PinnedPeerName:     "linux-gpu",
				PinnedPeerStatus:   "ok",
			})
		})
		if !strings.Contains(out, "worker:") || !strings.Contains(out, "linux-gpu") {
			t.Errorf("pinned output missing peer info: %q", out)
		}
	})
}
