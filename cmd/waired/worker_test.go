package main

import (
	"bytes"
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

// TestWorkerSet_ModePeerOnly covers the mode added in #327 reaching the
// daemon over the wire — the CLI validates --mode locally, so an
// unlisted value never gets that far.
func TestWorkerSet_ModePeerOnly(t *testing.T) {
	srv, spy := workerTestServer(t, inferencemesh.Snapshot{})
	defer srv.Close()
	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--mode=peer-only"}); err != nil {
			t.Fatalf("runWorker set: %v", err)
		}
	})
	if spy.posts[0].Mode != state.RoutingModePeerOnly {
		t.Errorf("mode = %q, want peer-only", spy.posts[0].Mode)
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
		state.RoutingModeAuto, state.RoutingModeLocalOnly,
		state.RoutingModePeerPreferred, state.RoutingModePeerOnly,
	}
	for _, m := range cases {
		t.Run(string(m), func(t *testing.T) {
			out := captureStdout(t, func() {
				printWorkerResponse(os.Stdout, management.WorkerResponse{Mode: m})
			})
			if !strings.Contains(out, fmt.Sprintf("mode:           %s", m)) {
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

// waired#1064: `waired worker get` names the model the pin resolves to,
// and says why it is not serving when it is not. Before this the command
// answered neither, so an operator had to cross-reference `peers list`.
func TestPrintWorkerResponse_PinnedNamesTheModelAndTheReason(t *testing.T) {
	render := func(resp management.WorkerResponse) string {
		t.Helper()
		return captureStdout(t, func() { printWorkerResponse(os.Stdout, resp) })
	}

	out := render(management.WorkerResponse{
		Mode:                state.RoutingModePinned,
		PinnedPeerDeviceID:  "dev_abc",
		PinnedPeerName:      "linux-gpu",
		PinnedPeerStatus:    "ok",
		PinnedPeerModel:     "qwen3-8b-instruct",
		PinnedPeerCondition: signer.SubsystemStateReady,
	})
	// The hand-padded label column is 13 wide; a new row that does not
	// line up with mode:/worker:/status: is the visible defect here.
	if !strings.Contains(out, "model:          qwen3-8b-instruct\n") {
		t.Errorf("model row missing or misaligned: %q", out)
	}
	if !strings.Contains(out, "status:         ok (peer reachable, serving)") {
		t.Errorf("ok status changed: %q", out)
	}

	out = render(management.WorkerResponse{
		Mode:                state.RoutingModePinned,
		PinnedPeerDeviceID:  "dev_abc",
		PinnedPeerName:      "linux-gpu",
		PinnedPeerStatus:    "unavailable",
		PinnedPeerModel:     "qwen3-8b-instruct",
		PinnedPeerCondition: signer.SubsystemStatePullFailed,
	})
	if !strings.Contains(out, "status:         unavailable (its model download failed)") {
		t.Errorf("condition not spelled out: %q", out)
	}

	// A peer that gave no reason keeps the wording it had before, and a
	// peer that named no model says so rather than printing a blank.
	out = render(management.WorkerResponse{
		Mode:               state.RoutingModePinned,
		PinnedPeerDeviceID: "dev_abc",
		PinnedPeerStatus:   "unavailable",
	})
	if !strings.Contains(out, "status:         unavailable (peer present but not serving)") {
		t.Errorf("older-peer status changed: %q", out)
	}
	if !strings.Contains(out, "model:          unknown") {
		t.Errorf("unnamed model rendered blank: %q", out)
	}
}

// The public-share fixtures. Deliberately unmistakable so a leak shows
// up as a substring hit, and synthetic because this repository is public
// (CLAUDE.md: never commit real device identifiers, including in test
// fixtures).
const (
	foreignDeviceID = "dev_foreign00000001"
	foreignAlias    = "guest-a7f3"
)

// PRODUCT CONTRACT (waired-agent#739 + public share spec §8.5, quoted in
// internal/gateway/probe.go's peerDisplayID doc). The ambiguity error is
// a CLI surface, so a public machine among the candidates is named by
// its grant pseudonym and never by its real device id.
//
// `waired ping` settled the same rule first
// (cmd/waired-agent/ping_peer_test.go's
// TestAgentPinger_AmbiguousNameNeverPrintsAGrantedPeersDeviceID, #723),
// and that test's comment names this command as the remaining leak.
func TestWorkerSet_PinAmbiguousNameNeverPrintsAPublicMachinesDeviceID(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev_mine", DeviceName: "shared-box", InferenceState: &signer.InferenceState{Reachable: true}},
			{
				DeviceID: foreignDeviceID, DeviceName: "shared-box",
				Grant:          &signer.PeerGrant{Kind: "public", Role: "provider", Pseudonym: foreignAlias},
				InferenceState: &signer.InferenceState{Reachable: true},
			},
		},
	}
	srv, _ := workerTestServer(t, snap)
	defer srv.Close()

	var err error
	_ = captureStdout(t, func() {
		err = runWorker([]string{"set", "--mgmt", srv.URL, "--pin=shared-box"})
	})
	if err == nil {
		t.Fatal("expected error for ambiguous name")
	}
	if strings.Contains(err.Error(), foreignDeviceID) {
		t.Errorf("the public machine's real device id crossed to the CLI: %v", err)
	}
	if !strings.Contains(err.Error(), foreignAlias) {
		t.Errorf("the public machine is not identified at all, so the operator cannot act: %v", err)
	}
	// The device the operator does own is still named outright.
	if !strings.Contains(err.Error(), "dev_mine") {
		t.Errorf("own-network candidate is missing: %v", err)
	}
}

// The pin VALUE stays the real DeviceID: it keys SetPin server-side and
// the router matches candidates on it. Scrubbing the display without
// keeping the key real is the one way this change could break routing,
// so the POSTed body is pinned separately from the printed output.
func TestWorkerSet_PinPublicMachineByNameSendsTheRealDeviceID(t *testing.T) {
	snap := inferencemesh.Snapshot{
		Peers: []inferencemesh.PeerView{{
			DeviceID: foreignDeviceID, DeviceName: foreignAlias,
			Grant:          &signer.PeerGrant{Kind: "public", Role: "provider", Pseudonym: foreignAlias},
			InferenceState: &signer.InferenceState{Reachable: true},
		}},
	}
	srv, spy := workerTestServer(t, snap)
	defer srv.Close()

	_ = captureStdout(t, func() {
		if err := runWorker([]string{"set", "--mgmt", srv.URL, "--pin=" + foreignAlias}); err != nil {
			t.Fatalf("worker set --pin: %v", err)
		}
	})
	if len(spy.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(spy.posts))
	}
	if got := spy.posts[0].PinnedPeerDeviceID; got != foreignDeviceID {
		t.Errorf("pinned_peer_device_id = %q, want the real device id %q — the daemon keys on it",
			got, foreignDeviceID)
	}
}

// The read-back is a CLI surface too, and it needs no name collision to
// reach: pin a public machine by name and `worker get` prints whatever
// the daemon returned. displayPin prefers the daemon's display
// identifier and falls back to the device id only for an agent that
// predates the field (#739).
func TestDisplayPin_PrefersTheDisplayIdentifier(t *testing.T) {
	cases := []struct {
		name string
		resp management.WorkerResponse
		want string
	}{
		{
			name: "public machine names the pseudonym, not the device id",
			resp: management.WorkerResponse{
				PinnedPeerDeviceID:  foreignDeviceID,
				PinnedPeerDisplayID: foreignAlias,
				PinnedPeerName:      foreignAlias,
			},
			want: foreignAlias + " (" + foreignAlias + ")",
		},
		{
			name: "own peer reads exactly as it did before",
			resp: management.WorkerResponse{
				PinnedPeerDeviceID:  "dev_lin",
				PinnedPeerDisplayID: "dev_lin",
				PinnedPeerName:      "linux-gpu",
			},
			want: "linux-gpu (dev_lin)",
		},
		{
			// An agent predating PinnedPeerDisplayID reports none.
			name: "no display identifier reported falls back to the device id",
			resp: management.WorkerResponse{PinnedPeerDeviceID: "dev_lin", PinnedPeerName: "linux-gpu"},
			want: "linux-gpu (dev_lin)",
		},
		{
			name: "no name reported shows the display identifier alone",
			resp: management.WorkerResponse{PinnedPeerDeviceID: foreignDeviceID, PinnedPeerDisplayID: foreignAlias},
			want: foreignAlias,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayPin(tc.resp); got != tc.want {
				t.Errorf("displayPin() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkerSet_PreferAndMinModelSize covers the two ordering flags of
// waired-agent#1128. What matters on this surface is the tri-state:
// `--min-model-size=""` is how an operator CLEARS the floor, and that has
// to reach the daemon as a value rather than as an absence.
func TestWorkerSet_PreferAndMinModelSize(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		mode    string
		pin     string
		prefer  *string
		minSize *string
		wantErr string
		check   func(*testing.T, management.WorkerRequest)
	}{
		{
			name: "prefer alone leaves the mode unsaid",
			// A request that names no mode leaves the mode and any pin
			// alone — the daemon reads the absence, so the CLI must not
			// fill it in.
			prefer: str("size"),
			check: func(t *testing.T, r management.WorkerRequest) {
				if r.Mode != "" {
					t.Errorf("Mode = %q, want unset", r.Mode)
				}
				if r.Prefer == nil || *r.Prefer != state.RoutingPreferSize {
					t.Errorf("Prefer = %v, want size", r.Prefer)
				}
				payload, err := json.Marshal(r)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if strings.Contains(string(payload), `"mode"`) {
					t.Errorf("body carries a mode key: %s", payload)
				}
			},
		},
		{
			name:    "an empty floor is a value, not an absence",
			minSize: str(""),
			check: func(t *testing.T, r management.WorkerRequest) {
				if r.MinModelSize == nil {
					t.Fatal("MinModelSize is nil; the clear would read as 'not supplied'")
				}
				if *r.MinModelSize != "" {
					t.Errorf("MinModelSize = %q, want empty", *r.MinModelSize)
				}
			},
		},
		{
			name:    "case and spacing are forgiven, as on public use",
			minSize: str("  Medium "), prefer: str("SPEED"),
			check: func(t *testing.T, r management.WorkerRequest) {
				if *r.MinModelSize != "medium" || *r.Prefer != state.RoutingPreferSpeed {
					t.Errorf("got %q / %v", *r.MinModelSize, *r.Prefer)
				}
			},
		},
		{
			name: "a mode and a preference together",
			mode: "local-only", prefer: str("speed"),
			check: func(t *testing.T, r management.WorkerRequest) {
				if r.Mode != state.RoutingModeLocalOnly || r.Prefer == nil {
					t.Errorf("got %+v, want both", r)
				}
			},
		},
		{
			name:    "an unknown prefer is refused locally",
			prefer:  str("quality"),
			wantErr: "--prefer must be speed or size",
		},
		{
			name:    "an unknown size is refused locally, in public use's words",
			minSize: str("enormous"),
			wantErr: "must be small, medium or large",
		},
		{
			name:    "nothing at all",
			wantErr: "pass --mode, --pin, --prefer or --min-model-size",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildWorkerRequest("", c.mode, c.pin, c.prefer, c.minSize)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildWorkerRequest: %v", err)
			}
			c.check(t, got)
		})
	}
}

// TestPrintWorkerResponse_ReportsTheOrderingPreferences: an operator who
// cannot see the setting cannot tell whether it took.
func TestPrintWorkerResponse_ReportsTheOrderingPreferences(t *testing.T) {
	var buf bytes.Buffer
	printWorkerResponse(&buf, management.WorkerResponse{
		Mode: state.RoutingModeAuto, Prefer: state.RoutingPreferSize, MinModelSize: "medium",
	})
	out := buf.String()
	if !strings.Contains(out, "prefer:         size\n") {
		t.Errorf("prefer row missing or misaligned: %q", out)
	}
	if !strings.Contains(out, "smallest model: medium\n") {
		t.Errorf("smallest-model row missing or misaligned: %q", out)
	}

	// An agent predating the field sends neither. The defaults are what
	// the ordering actually does, so that is what is printed.
	buf.Reset()
	printWorkerResponse(&buf, management.WorkerResponse{Mode: state.RoutingModeAuto})
	out = buf.String()
	if !strings.Contains(out, "prefer:         speed\n") {
		t.Errorf("an unset prefer must print the default: %q", out)
	}
	if !strings.Contains(out, "smallest model: any\n") {
		t.Errorf("an unset floor must print 'any', as `waired public use` does: %q", out)
	}
}
