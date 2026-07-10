package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

func peersTestServer(t *testing.T, snap inferencemesh.Snapshot) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/mesh", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(snap)
	})
	return httptest.NewServer(mux)
}

func TestPeersList_TableIncludesPeerColumns(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_linux",
				DeviceName: "linux-gpu",
				OverlayIP:  "10.42.0.2",
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Models:    []string{"qwen3:8b-q4_K_M"},
					Hardware: &signer.HardwareSummary{
						GPUs: []signer.HardwareGPUSummary{{Model: "RTX 4090", VRAMTotalMB: 24576}},
					},
				},
			},
		},
	}
	srv := peersTestServer(t, snap)
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL)}); err != nil {
			t.Fatalf("runPeers list: %v", err)
		}
	})
	for _, want := range []string{
		"NAME", "DEVICE-ID", "OVERLAY-IP", "ENGINE", "GPU", "VRAM", "MODELS", "WORKER-CAPABLE",
		"linux-gpu", "dev_linux", "10.42.0.2", "ollama", "RTX 4090", "qwen3:8b-q4_K_M", "yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestPeersList_FlagsUnreachableAsNotCapable(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_down",
				DeviceName: "alice",
				InferenceState: &signer.InferenceState{
					Reachable: false,
					Type:      signer.InferenceTypeOllama,
				},
			},
			{
				DeviceID:   "dev_no_engine",
				DeviceName: "bob",
				// InferenceState nil → no engine advertised
			},
		},
	}
	srv := peersTestServer(t, snap)
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL)}); err != nil {
			t.Fatalf("runPeers list: %v", err)
		}
	})
	if !strings.Contains(out, "unreachable") {
		t.Errorf("unreachable peer not flagged: %q", out)
	}
	if !strings.Contains(out, "no engine") {
		t.Errorf("no-engine peer not flagged: %q", out)
	}
}

func TestPeersList_StalePeerFlagged(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_stale",
				DeviceName: "peer-stale",
				Stale:      true,
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Models:    []string{"qwen3:8b-q4_K_M"},
				},
			},
		},
	}
	srv := peersTestServer(t, snap)
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL)}); err != nil {
			t.Fatalf("runPeers list: %v", err)
		}
	})
	if !strings.Contains(out, "stale") {
		t.Errorf("stale peer not flagged: %q", out)
	}
}

func TestPeersList_EmptyMeshMessage(t *testing.T) {
	srv := peersTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL)}); err != nil {
			t.Fatalf("runPeers list: %v", err)
		}
	})
	if !strings.Contains(out, "no peers") {
		t.Errorf("empty mesh should say 'no peers', got %q", out)
	}
}

func TestPeersList_JSON(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev_a", DeviceName: "node", InferenceState: &signer.InferenceState{Reachable: true}},
		},
	}
	srv := peersTestServer(t, snap)
	defer srv.Close()
	out := captureStdout(t, func() {
		if err := runPeers([]string{"list", "--mgmt", meshAddrFromURL(srv.URL), "--json"}); err != nil {
			t.Fatalf("runPeers list --json: %v", err)
		}
	})
	var decoded struct {
		Peers []inferencemesh.PeerView `json:"peers"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output should be JSON: %v (raw=%s)", err, out)
	}
	if len(decoded.Peers) != 1 || decoded.Peers[0].DeviceID != "dev_a" {
		t.Errorf("decoded = %+v", decoded.Peers)
	}
}

func TestPeers_UnknownSubcommandRejected(t *testing.T) {
	var err error
	_ = captureStdout(t, func() {
		err = runPeers([]string{"oops"})
	})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}
