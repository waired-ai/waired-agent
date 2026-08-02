package main

// These tests pin the MOCK's contract, not the tray's rendering: they assert
// that what this fixture puts on the wire decodes into the very structs the
// tray decodes into, with the field values each menu branch needs. The tray's
// projection of those structs is pinned separately by
// internal/gui/tray/state_worker_test.go — deliberately not imported here,
// because pulling internal/gui/tray into this package (even from a _test
// file) breaks `GOOS=darwin go vet $(DARWIN_VET_PKGS)`, which runs with CGO
// off and cannot see systray's Cocoa symbols.
//
// PRODUCT CONTRACT, not a record of today's behaviour: a screenshot session
// is worthless if the fixture cannot produce the state being screenshotted,
// and a JSON key typo here renders as "the menu is missing" — which reads as
// a tray bug. That is the failure these tests exist to make loud.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func newTestServer(t *testing.T, n routingName) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMockServer(stateConnected, n).mux())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, out any) int {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func post(t *testing.T, srv *httptest.Server, path, body string, out any) int {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("POST %s: decode: %v", path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

// The tray reads the routing mode off /inference/status, NOT off
// /waired/v1/worker (management.InferenceStatus.Worker) — a `worker` key
// that fails to decode leaves Snapshot.Inference.Worker nil and silently
// hides the whole "Inference routing" parent.
func TestInferenceStatusCarriesWorkerForTray(t *testing.T) {
	srv := newTestServer(t, routingAuto)

	var got management.InferenceStatus
	if code := get(t, srv, "/waired/v1/inference/status", &got); code != http.StatusOK {
		t.Fatalf("status: HTTP %d, want 200", code)
	}
	if got.Worker == nil {
		t.Fatal("inference/status has no decodable `worker` key: the tray would hide Inference routing")
	}
	if got.Worker.Mode != state.RoutingModeAuto {
		t.Errorf("worker.mode = %q, want %q", got.Worker.Mode, state.RoutingModeAuto)
	}
}

// One snapshot has to be able to produce all three pin-row shapes at once —
// serving, unavailable, and excluded — or the operator has to restart the
// mock to see each of them.
func TestMeshSnapshotCoversEveryPinRowShape(t *testing.T) {
	srv := newTestServer(t, routingAuto)

	var snap inferencemesh.Snapshot
	if code := get(t, srv, "/waired/v1/inference/mesh", &snap); code != http.StatusOK {
		t.Fatalf("mesh: HTTP %d, want 200", code)
	}
	if !snap.Reachable {
		t.Error("snapshot.reachable = false; the tray would render \"no reachable peer engine\"")
	}

	var serving, unavailable, excluded int
	for _, p := range snap.Peers {
		switch {
		case p.InferenceState == nil || p.InferenceState.Type == "" ||
			p.InferenceState.Type == "none":
			excluded++ // peerIsInferenceCandidate drops these
		case p.Stale || !p.InferenceState.Reachable || len(p.InferenceState.Models) == 0:
			unavailable++
		default:
			serving++
		}
	}
	if serving < 1 || unavailable < 1 || excluded != 1 {
		t.Errorf("peer mix = %d serving / %d unavailable / %d excluded; want >=1 / >=1 / exactly 1",
			serving, unavailable, excluded)
	}

	// waired-agent#326: the aggregate is name-sorted, and that ordering is
	// what keeps a peer on the same menu row poll over poll. A mock that
	// emitted them unordered would re-create the blinking rows the tray was
	// fixed for.
	for i := 1; i < len(snap.Peers); i++ {
		if snap.Peers[i-1].DeviceName > snap.Peers[i].DeviceName {
			t.Fatalf("peers are not name-sorted at %d: %q then %q",
				i, snap.Peers[i-1].DeviceName, snap.Peers[i].DeviceName)
		}
	}

	// Self must never be counted into the peers-only aggregate.
	for _, p := range snap.Peers {
		if p.DeviceID == snap.SelfDeviceID {
			t.Errorf("self %q appears in peers", p.DeviceID)
		}
	}
}

// Every mode row in the menu POSTs one of these, and the tray re-polls right
// after; a mode the mock silently dropped would look like a dead menu item.
func TestWorkerPostRoundTripsEveryMode(t *testing.T) {
	for _, mode := range []state.RoutingMode{
		state.RoutingModeAuto,
		state.RoutingModeLocalOnly,
		state.RoutingModePeerPreferred,
		state.RoutingModePeerOnly,
	} {
		t.Run(string(mode), func(t *testing.T) {
			srv := newTestServer(t, routingAuto)

			var posted management.WorkerResponse
			if code := post(t, srv, "/waired/v1/worker",
				`{"mode":"`+string(mode)+`"}`, &posted); code != http.StatusOK {
				t.Fatalf("POST worker: HTTP %d, want 200", code)
			}
			if posted.Mode != mode {
				t.Errorf("POST echoed mode %q, want %q", posted.Mode, mode)
			}

			// The tray refreshes from /inference/status, so the mutation has
			// to be visible there and not only in the POST's own response.
			var status management.InferenceStatus
			get(t, srv, "/waired/v1/inference/status", &status)
			if status.Worker == nil || status.Worker.Mode != mode {
				t.Errorf("inference/status worker = %+v, want mode %q", status.Worker, mode)
			}
		})
	}
}

// Pinning is the one mode that carries a device ID, and the summary row's
// suffix is derived from the mesh — not echoed back from the request.
func TestWorkerPinDerivesNameAndStatusFromMesh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deviceID   string
		wantName   string
		wantStatus string
	}{
		{"serving peer", "dev-mock-mini", "mock-mini", "ok"},
		{"stale peer", "dev-mock-nuc", "mock-nuc", "unavailable"},
		{"peer not in the mesh", "dev-mock-gone", "", "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, routingAuto)

			var got management.WorkerResponse
			if code := post(t, srv, "/waired/v1/worker",
				`{"mode":"pinned","pinned_peer_device_id":"`+tc.deviceID+`"}`,
				&got); code != http.StatusOK {
				t.Fatalf("POST worker: HTTP %d, want 200", code)
			}
			if got.PinnedPeerName != tc.wantName || got.PinnedPeerStatus != tc.wantStatus {
				t.Errorf("pin = (%q, %q), want (%q, %q)",
					got.PinnedPeerName, got.PinnedPeerStatus, tc.wantName, tc.wantStatus)
			}
		})
	}
}

// Mirrors management.Server.applyWorkerRequest. A mock that accepted these
// would make a tray-side bug indistinguishable from a fixture-side one.
func TestWorkerPostRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"pinned without a device id", `{"mode":"pinned"}`},
		{"auto with a device id", `{"mode":"auto","pinned_peer_device_id":"dev-mock-mini"}`},
		{"peer-only with a device id", `{"mode":"peer-only","pinned_peer_device_id":"dev-mock-mini"}`},
		{"unknown mode", `{"mode":"teleport"}`},
		{"malformed body", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, routingAuto)
			if code := post(t, srv, "/waired/v1/worker", tc.body, nil); code != http.StatusBadRequest {
				t.Errorf("HTTP %d, want 400", code)
			}
		})
	}
}

func TestWorkerRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t, routingAuto)
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/waired/v1/worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT /worker: HTTP %d, want 405", resp.StatusCode)
	}
}

func TestMeshRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t, routingAuto)
	if code := post(t, srv, "/waired/v1/inference/mesh", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /inference/mesh: HTTP %d, want 405", code)
	}
}

// The scenario table is the operator's menu of screenshots; each row has to
// actually produce the shape its name promises.
func TestRoutingScenarioShapes(t *testing.T) {
	for _, tc := range []struct {
		scenario      routingName
		wantWorkerKey bool
		wantWorkerGET int
		wantMeshGET   int
		wantPeers     int
		wantReachable bool
		wantMode      state.RoutingMode
		wantPinStatus string
	}{
		{routingAuto, true, 200, 200, 4, true, state.RoutingModeAuto, ""},
		{routingPeerOnly, true, 200, 200, 4, true, state.RoutingModePeerOnly, ""},
		{routingPinned, true, 200, 200, 4, true, state.RoutingModePinned, "ok"},
		{routingPinnedUnavailable, true, 200, 200, 4, true, state.RoutingModePinned, "unavailable"},
		{routingPinnedAbsent, true, 200, 200, 4, true, state.RoutingModePinned, "absent"},
		{routingPeersDown, true, 200, 200, 4, false, state.RoutingModeAuto, ""},
		{routingNoPeers, true, 200, 200, 0, false, state.RoutingModeAuto, ""},
		{routingMeshOff, true, 200, 404, 0, false, state.RoutingModeAuto, ""},
		{routingWorkerOff, false, 404, 200, 4, true, "", ""},
		{routingOff, false, 404, 404, 0, false, "", ""},
	} {
		t.Run(string(tc.scenario), func(t *testing.T) {
			srv := newTestServer(t, tc.scenario)

			var status management.InferenceStatus
			get(t, srv, "/waired/v1/inference/status", &status)
			if hasWorker := status.Worker != nil; hasWorker != tc.wantWorkerKey {
				t.Fatalf("inference/status worker key present = %v, want %v", hasWorker, tc.wantWorkerKey)
			}
			if tc.wantWorkerKey {
				if status.Worker.Mode != tc.wantMode {
					t.Errorf("mode = %q, want %q", status.Worker.Mode, tc.wantMode)
				}
				if status.Worker.PinnedPeerStatus != tc.wantPinStatus {
					t.Errorf("pinned_peer_status = %q, want %q",
						status.Worker.PinnedPeerStatus, tc.wantPinStatus)
				}
			}

			if code := get(t, srv, "/waired/v1/worker", nil); code != tc.wantWorkerGET {
				t.Errorf("GET /worker: HTTP %d, want %d", code, tc.wantWorkerGET)
			}

			var snap inferencemesh.Snapshot
			code := get(t, srv, "/waired/v1/inference/mesh", &snap)
			if code != tc.wantMeshGET {
				t.Fatalf("GET /inference/mesh: HTTP %d, want %d", code, tc.wantMeshGET)
			}
			if code != http.StatusOK {
				return
			}
			if len(snap.Peers) != tc.wantPeers {
				t.Errorf("peers = %d, want %d", len(snap.Peers), tc.wantPeers)
			}
			if snap.Reachable != tc.wantReachable {
				t.Errorf("reachable = %v, want %v", snap.Reachable, tc.wantReachable)
			}
		})
	}
}

// peers-down is the scenario for "everything the operator could pin is
// currently down", so every candidate row must read unavailable — not just
// the one that is stale in the base fixture.
func TestPeersDownMakesEveryCandidateUnavailable(t *testing.T) {
	srv := newTestServer(t, routingPeersDown)

	var snap inferencemesh.Snapshot
	get(t, srv, "/waired/v1/inference/mesh", &snap)
	for _, p := range snap.Peers {
		if p.InferenceState == nil {
			continue
		}
		if !p.Stale {
			t.Errorf("peer %q is not stale under peers-down", p.DeviceName)
		}
	}
}

func TestSetStateFlipsBothAxes(t *testing.T) {
	srv := newTestServer(t, routingAuto)

	if code := post(t, srv, "/_mock/state?value=paused&routing=peer-only", "", nil); code != http.StatusOK {
		t.Fatalf("_mock/state: HTTP %d, want 200", code)
	}

	var status map[string]any
	get(t, srv, "/waired/v1/status", &status)
	if status["phase"] != "paused" {
		t.Errorf("phase = %v, want paused", status["phase"])
	}

	var inf management.InferenceStatus
	get(t, srv, "/waired/v1/inference/status", &inf)
	if inf.Worker == nil || inf.Worker.Mode != state.RoutingModePeerOnly {
		t.Errorf("worker = %+v, want mode peer-only", inf.Worker)
	}
}

// Flipping the scenario has to discard clicks, otherwise a screenshot run
// inherits whatever the previous scenario was left pinned to.
func TestSetRoutingResetsTheWorkerChoice(t *testing.T) {
	srv := newTestServer(t, routingAuto)

	post(t, srv, "/waired/v1/worker", `{"mode":"pinned","pinned_peer_device_id":"dev-mock-studio"}`, nil)
	post(t, srv, "/_mock/state?routing=auto", "", nil)

	var inf management.InferenceStatus
	get(t, srv, "/waired/v1/inference/status", &inf)
	if inf.Worker == nil || inf.Worker.Mode != state.RoutingModeAuto || inf.Worker.PinnedPeerDeviceID != "" {
		t.Errorf("worker = %+v, want a clean auto", inf.Worker)
	}
}

func TestSetStateRejectsUnknownValues(t *testing.T) {
	srv := newTestServer(t, routingAuto)
	for _, q := range []string{"?value=nonsense", "?routing=nonsense", ""} {
		if code := post(t, srv, "/_mock/state"+q, "", nil); code != http.StatusBadRequest {
			t.Errorf("_mock/state%s: HTTP %d, want 400", q, code)
		}
	}
}
