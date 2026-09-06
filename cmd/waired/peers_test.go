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
					// waired#1064: the MODEL column is the catalog id and
					// MODELS keeps the engine tag, because only the first
					// is comparable across a mixed fleet and only the
					// second is what a request is matched against.
					ActiveModel:    "qwen3-8b-instruct",
					SubsystemState: signer.SubsystemStateReady,
					Hardware: &signer.HardwareSummary{
						GPUs: []signer.HardwareGPUSummary{{Model: "RTX 4090", VRAMTotalMB: 24576}},
					},
				},
			},
			// A peer mid-download: its engine tag is withdrawn, so before
			// waired#1064 the row read exactly like a dead engine's.
			{
				DeviceID:   "dev_busy",
				DeviceName: "mac-studio",
				OverlayIP:  "10.42.0.3",
				InferenceState: &signer.InferenceState{
					Reachable:      true,
					Type:           signer.InferenceTypeOllama,
					ActiveModel:    "qwen3-coder-next-80b-a3b-instruct",
					SubsystemState: signer.SubsystemStateLoading,
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
		"NAME", "DEVICE-ID", "OVERLAY-IP", "ENGINE", "MODEL", "GPU", "VRAM", "MODELS", "WORKER-CAPABLE",
		"linux-gpu", "dev_linux", "10.42.0.2", "ollama", "RTX 4090", "qwen3:8b-q4_K_M", "yes",
		"qwen3-8b-instruct",
		// The downloading peer names its model and says why it cannot
		// serve, where it used to read "no (no model)".
		"mac-studio", "qwen3-coder-next-80b-a3b-instruct", "no (downloading)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestPeersList_UnifiedMemoryPeerShowsItsUsableBound pins that the VRAM
// column has a figure for a peer whose GPU shares system RAM.
//
// PRODUCT CONTRACT (waired-ai/waired-agent#662). Apple Silicon publishes
// no per-GPU total — its detector leaves that field 0 deliberately,
// because the honest bound there is the OS-reserved usable figure — so
// this column read "-" for an M-series Mac while an AMD Strix Halo on the
// same mesh showed 96 GB. Both hosts below are unified-memory; the
// difference is only which field carries the number.
func TestPeersList_UnifiedMemoryPeerShowsItsUsableBound(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_mac",
				DeviceName: "mac-mini",
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Hardware: &signer.HardwareSummary{
						// What a real M4 sends: a named GPU with no
						// per-device total, and the budget at the summary
						// level.
						GPUs:          []signer.HardwareGPUSummary{{Model: "Apple M4", Vendor: "apple"}},
						RAMTotalGB:    16,
						UnifiedMemory: true,
						UsableVRAMMB:  12288,
					},
				},
			},
			{
				DeviceID:   "dev_amd",
				DeviceName: "strix",
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Hardware: &signer.HardwareSummary{
						// rocm-smi reports the whole BIOS carve-out as the
						// device total, so this host has always had a
						// figure in the per-GPU field.
						GPUs:          []signer.HardwareGPUSummary{{Model: "AMD Radeon(TM) 8060S Graphics", Vendor: "amd", VRAMTotalMB: 98304}},
						RAMTotalGB:    128,
						UnifiedMemory: true,
						UsableVRAMMB:  98304,
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
	for _, want := range []string{"Apple M4", "12 GB", "AMD Radeon(TM) 8060S Graphics", "96 GB"} {
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
	// The peer's own engine probe came back empty. #849: the old word
	// here was "unreachable", which reads as "this computer cannot get to
	// it" — a different fact, and one this column never held.
	if !strings.Contains(out, "no (engine not answering)") {
		t.Errorf("peer whose own engine did not answer not flagged: %q", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("column still claims a viewer-side verdict it never made: %q", out)
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

// TestPeersList_StaleNoteAnswersBothQuestions covers what the word alone
// did not (#661): how old is old enough, and whether the row ever goes
// away. A peer offline for nine days still listed as `no (stale)` reads
// like a broken listing rather than a departed peer.
//
// The window comes from the snapshot, so the printed number is the one the
// daemon is applying rather than a constant this process guessed.
func TestPeersList_StaleNoteAnswersBothQuestions(t *testing.T) {
	snap := inferencemesh.Snapshot{
		StalenessThresholdMS: 90_000,
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
	if !strings.Contains(out, "1m30s") {
		t.Errorf("note does not state the threshold the daemon reported: %q", out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("note does not say when the row goes away: %q", out)
	}
}

// TestPeersList_NoStaleNoteWhenNothingIsStale keeps the note out of the
// common case: a footnote on every listing is noise, and it would explain
// a word that is not on screen.
func TestPeersList_NoStaleNoteWhenNothingIsStale(t *testing.T) {
	snap := inferencemesh.Snapshot{
		StalenessThresholdMS: 90_000,
		Peers: []inferencemesh.PeerView{
			{
				DeviceID:   "dev_ok",
				DeviceName: "peer-ok",
				InferenceState: &signer.InferenceState{
					Reachable:      true,
					Type:           signer.InferenceTypeOllama,
					Models:         []string{"qwen3:8b-q4_K_M"},
					SubsystemState: signer.SubsystemStateReady,
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
	if strings.Contains(out, "however long they have been offline") {
		t.Errorf("stale note printed with no stale peer: %q", out)
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
	if !strings.Contains(out, "no other computers") {
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

// TestPeers_BareCommandFailsAndPrintsHelp: naming a namespace where a verb
// belongs is a mistake, and it used to exit 0 after printing help — which
// made `waired peers` indistinguishable from a listing that found nothing.
// A script got success and no data (#661).
//
// Help still prints, because the fix is to pick a subcommand and that is
// the list of them.
//
// Product contract, ratified by #661 and the owner's call to make the bare
// form fail.
func TestPeers_BareCommandFailsAndPrintsHelp(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = runPeers(nil)
	})
	if err == nil {
		t.Fatal("bare `waired peers` returned nil; it must fail rather than look like an empty listing")
	}
	if !strings.Contains(out, "list") {
		t.Errorf("help did not name the subcommand to use: %q", out)
	}
}
