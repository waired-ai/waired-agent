// Command mock-mgmt stands in for the agent's Local Management API
// for the limited purpose of driving the desktop tray during UI
// screenshot verification.
//
// It is NOT a fixture for unit tests (the tray package has its own
// httptest-based tests); the point of this binary is to give the
// human running `make build-tray-darwin` — or the Windows review host —
// a real HTTP server they can swap into a particular fake state and
// screenshot the tray's rendering against.
//
// # Two axes
//
// Connection state and inference-routing state are independent, and both
// can be flipped at runtime without a restart:
//
//	-state   / curl -XPOST '127.0.0.1:9476/_mock/state?value=paused'
//	-routing / curl -XPOST '127.0.0.1:9476/_mock/state?routing=pinned'
//
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state connected
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state disconnected
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state paused
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state error
//
// # Why the routing axis exists
//
// The "Inference routing" submenu (waired-agent#327) is driven by two
// endpoints — the `worker` key of GET /waired/v1/inference/status and
// GET /waired/v1/inference/mesh — and tray.Update() only shows that
// top-level parent when at least one of them answers:
//
//	ShowRoutingMenu = ShowWorker || MeshReachableLabel != ""
//
// Until this mock served them, pointing the tray at it rendered no
// routing menu at all, so the approved menu shape had never actually
// been seen on any OS (Linux CI cannot draw a tray, and the tray's own
// unit tests can only exercise one row at a time — see rows.go).
//
// # Scenarios
//
// Each -routing value is one {starting worker choice, mesh answer} pair.
// Together they cover every branch of applyWorker / applyMeshReachable:
//
//	auto                (default) auto mode + 4 peers, 2 of them serving
//	peer-only           peer-only mode selected (the #327 addition)
//	pinned              pinned to a serving peer
//	pinned-unavailable  pinned to a stale peer   → "… (pinned) (unavailable)"
//	pinned-absent       pinned to a device the mesh does not carry → "(absent)"
//	peers-down          every peer stale → "Mesh: no reachable peer engine"
//	no-peers            empty peer list → no pin rows AND no "Pin to one peer"
//	mesh-off            /inference/mesh 404s (daemon predates the mesh API)
//	worker-off          /worker 404s + no `worker` key → only the mesh row
//	off                 both 404 → the whole "Inference routing" parent hides
//
// The scenario only picks the STARTING worker choice. Clicking a mode or
// pin row in the tray POSTs /waired/v1/worker, which this mock applies
// (with the daemon's own validation) so the ●/○ moves on the next poll.
// curl drives the same endpoint for scripted setup:
//
//	curl -XPOST 127.0.0.1:9476/waired/v1/worker \
//	  -d '{"mode":"pinned","pinned_peer_device_id":"dev-mock-studio"}'
//
// The routing rows render only while the menu is Connected or
// Disconnected, i.e. under -state connected or -state paused.
//
// # Runbook (screenshot verification)
//
//	make build-tray-darwin                 # or the Windows review host's .exe
//	go run ./scripts/dev/mock-mgmt -socket /tmp/waired-mock.sock -routing auto &
//	WAIRED_MGMT_SOCKET=/tmp/waired-mock.sock ./bin/waired-tray
//	curl -XPOST '127.0.0.1:9476/_mock/state?routing=pinned-unavailable'
//
// Reads go to the loopback TCP port; writes go over the IPC socket
// (waired#838), which is why -socket is needed for the clicks to land.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// connState is the tunnel/enrolment axis. Named for what it is now that
// routingName is a second, independent axis on the same server.
type connState string

const (
	stateConnected    connState = "connected"
	stateDisconnected connState = "disconnected"
	statePaused       connState = "paused"
	stateError        connState = "error"
)

const (
	selfDeviceID   = "dev-mock-laptop"
	selfDeviceName = "mock-laptop"
)

// routingName selects one row of routingScenarios.
type routingName string

const (
	routingAuto              routingName = "auto"
	routingPeerOnly          routingName = "peer-only"
	routingPinned            routingName = "pinned"
	routingPinnedUnavailable routingName = "pinned-unavailable"
	routingPinnedAbsent      routingName = "pinned-absent"
	routingPeersDown         routingName = "peers-down"
	routingNoPeers           routingName = "no-peers"
	routingMeshOff           routingName = "mesh-off"
	routingWorkerOff         routingName = "worker-off"
	routingOff               routingName = "off"
)

// routingScenario is what the two routing endpoints answer. workerOff and
// meshOff model the two independent "older daemon" 404s the tray degrades
// against; the rest shape the mesh snapshot and the starting worker choice.
type routingScenario struct {
	workerOff bool // /waired/v1/worker 404s AND /inference/status omits "worker"
	meshOff   bool // /waired/v1/inference/mesh 404s
	noPeers   bool // snapshot carries an empty peer list
	peersDown bool // every peer is stale → every pin row reads "(unavailable)"

	mode routingMode
	pin  string

	descr string
}

// routingMode aliases the runtime routing mode so the scenario table reads
// as a table rather than a wall of package qualifiers.
type routingMode = state.RoutingMode

var routingScenarios = map[routingName]routingScenario{
	routingAuto: {
		mode: state.RoutingModeAuto, descr: "auto + 4 peers (2 serving, 1 stale, 1 engineless)",
	},
	routingPeerOnly: {
		mode: state.RoutingModePeerOnly, descr: "peer-only selected",
	},
	routingPinned: {
		mode: state.RoutingModePinned, pin: "dev-mock-mini", descr: "pinned to a serving peer",
	},
	routingPinnedUnavailable: {
		mode: state.RoutingModePinned, pin: "dev-mock-nuc", descr: "pinned to a stale peer → (unavailable)",
	},
	routingPinnedAbsent: {
		mode: state.RoutingModePinned, pin: "dev-mock-gone", descr: "pinned to a device the mesh does not carry → (absent)",
	},
	routingPeersDown: {
		peersDown: true, mode: state.RoutingModeAuto, descr: "every peer stale → no reachable peer engine",
	},
	routingNoPeers: {
		noPeers: true, mode: state.RoutingModeAuto, descr: "empty peer list → no pin rows, no pin header",
	},
	routingMeshOff: {
		meshOff: true, mode: state.RoutingModeAuto, descr: "mesh endpoint 404 (pre-mesh daemon)",
	},
	routingWorkerOff: {
		workerOff: true, mode: state.RoutingModeAuto, descr: "worker endpoint 404 → mesh row alone opens the parent",
	},
	routingOff: {
		workerOff: true, meshOff: true, mode: state.RoutingModeAuto, descr: "both 404 → routing parent hidden entirely",
	},
}

// peerFixture is one entry of the fake mesh. Kept as a table rather than a
// literal Snapshot so a scenario can re-derive the snapshot with different
// staleness without duplicating the list.
//
// engine == "" means the peer advertises no InferenceState at all: the tray's
// peerIsInferenceCandidate filter must drop it from the pin rows. It sits in
// the MIDDLE of the (name-sorted) list on purpose, so a filter that merely
// truncated the tail would be visible in a screenshot.
//
// No real device identifiers — this repo is public. Overlay addresses are
// from the CGNAT range the overlay itself uses.
type peerFixture struct {
	deviceID   string
	deviceName string
	overlayIP  string
	engine     string
	models     []string
	stale      bool
}

var meshPeers = []peerFixture{
	{
		deviceID: "dev-mock-mini", deviceName: "mock-mini", overlayIP: "100.64.0.11",
		engine: signer.InferenceTypeOllama, models: []string{"qwen3.6:35b-a3b"},
	},
	{
		deviceID: "dev-mock-nuc", deviceName: "mock-nuc", overlayIP: "100.64.0.12",
		engine: signer.InferenceTypeOllama, models: []string{"llama3.3:70b"}, stale: true,
	},
	{
		deviceID: "dev-mock-phone", deviceName: "mock-phone", overlayIP: "100.64.0.13",
	},
	{
		deviceID: "dev-mock-studio", deviceName: "mock-studio", overlayIP: "100.64.0.14",
		engine: signer.InferenceTypeVLLM, models: []string{"qwen3.6:35b-a3b-awq"},
	},
}

// mockServer holds the current fake state and serves the subset of
// /waired/v1/* endpoints the tray polls. Other endpoints return 404
// so the tray's Err*Unsupported fall-throughs fire and the relevant
// menu groups (Claude / OpenCode / OpenClaw / inference catalog) hide.
type mockServer struct {
	mu sync.RWMutex
	s  connState
	// Claude Code per-class routing policy (#649/#650) — mutated in place
	// by POST /waired/v1/integration/claude/route so the tray's ●/○ marks
	// move when the operator clicks.
	claudeMain string
	claudeSub  string
	// Inference routing (#327). routing picks the scenario; workerMode /
	// workerPin start from it and are then mutated by POST /waired/v1/worker
	// — i.e. by the operator clicking a mode or pin row.
	routing    routingName
	workerMode routingMode
	workerPin  string
	// Model residency (waired-agent#861). idle is the setting the
	// "Keep model in memory" rows render and mutate; resident is what the
	// "(loaded)" suffix and the unload row's enabled state read. Both are
	// mutated by the tray's own POSTs so the menu shape can actually be
	// watched changing — the rows are otherwise unobservable on any OS,
	// since Linux CI cannot draw a tray (#397).
	idle     time.Duration
	resident bool

	// engine picks the shape /inference/status reports for the runtimes[]
	// rows. Fixed for the life of the process — unlike the two axes above
	// it is not something a click moves.
	engine string
}

// engineLatchedStopped is the state waired-agent#1111 was filed for, and
// the one no healthy host can be coaxed into: a vLLM engine whose recovery
// budget ran out (failure_latched, with the reason on it) and which was then
// STOPPED — a model switch, a reconcile bounce, a park. Stop() overwrites the
// whole Health struct with no give-up guard, so the row reads "stopped" while
// failure_latched and last_error are both still on the wire, and no row reads
// "failed" at all. The ollama row beside it is the registered, never-started
// adapter every host carries, which is what the tray used to fall back to.
const engineLatchedStopped = "latched-stopped"

func main() {
	listen := flag.String("listen", "127.0.0.1:9476",
		"address to bind (default matches management.DefaultListen)")
	socket := flag.String("socket", "",
		"also serve the same mux on this unix-domain socket path; export WAIRED_MGMT_SOCKET=<path> so the tray/CLI send their writes here (waired#838)")
	initial := flag.String("state", "connected",
		"initial state: connected | disconnected | paused | error")
	routing := flag.String("routing", string(routingAuto),
		"initial inference-routing scenario: "+strings.Join(routingNames(), " | "))
	engine := flag.String("engine", "ready",
		"engine scenario: ready | latched-stopped (a vLLM host that gave up and was then stopped)")
	flag.Parse()

	sc, ok := routingScenarios[routingName(*routing)]
	if !ok {
		log.Fatalf("mock-mgmt: unknown -routing %q (want one of: %s)",
			*routing, strings.Join(routingNames(), ", "))
	}

	if *engine != "ready" && *engine != engineLatchedStopped {
		log.Fatalf("mock-mgmt: unknown -engine %q (want: ready | %s)", *engine, engineLatchedStopped)
	}

	srv := newMockServer(connState(*initial), routingName(*routing))
	srv.engine = *engine
	mux := srv.mux()

	// Since waired#838 the tray and CLI send MUTATING requests over a local
	// IPC socket rather than the loopback TCP port, so a TCP-only mock no
	// longer sees pause/resume/enable/disable — or the worker mode/pin
	// clicks. Serving the same mux on a unix socket keeps those paths
	// exercised; point clients at it with WAIRED_MGMT_SOCKET=<path>.
	if *socket != "" {
		_ = os.Remove(*socket)
		ln, err := net.Listen("unix", *socket)
		if err != nil {
			log.Fatalf("mock-mgmt: listen unix %s: %v", *socket, err)
		}
		if err := os.Chmod(*socket, 0o666); err != nil {
			log.Fatalf("mock-mgmt: chmod %s: %v", *socket, err)
		}
		fmt.Fprintf(os.Stderr, "mock-mgmt also serving writes on unix %s (export WAIRED_MGMT_SOCKET=%s)\n", *socket, *socket)
		go func() {
			sockSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
			log.Fatal(sockSrv.Serve(ln))
		}()
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "mock-mgmt listening on %s (state=%s, routing=%s — %s)\n",
		*listen, srv.s, *routing, sc.descr)
	log.Fatal(httpSrv.ListenAndServe())
}

// newMockServer seeds both axes. The worker choice starts at the scenario's
// value and then drifts as the operator clicks, which is why it is stored
// separately from the scenario rather than derived on every read.
func newMockServer(s connState, n routingName) *mockServer {
	sc := routingScenarios[n]
	return &mockServer{
		s:          s,
		claudeMain: "auto",
		claudeSub:  "same",
		routing:    n,
		workerMode: sc.mode,
		workerPin:  sc.pin,
		// Start held-indefinitely and loaded — the product default (owner
		// ruling on waired-agent#861), so the mock opens on the shape a
		// normal host actually has.
		idle:     0,
		resident: true,
	}
}

// mux is split out of main so the tests can drive the same routes through
// httptest without binding a port or a unix socket (the latter would not
// exist on the Windows unit leg).
func (m *mockServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", m.handleStatus)
	mux.HandleFunc("/waired/v1/identity", m.handleIdentity)
	mux.HandleFunc("/waired/v1/pause", m.handlePause)
	mux.HandleFunc("/waired/v1/resume", m.handleResume)
	mux.HandleFunc("/waired/v1/inference/status", m.handleInferenceStatus)
	mux.HandleFunc("/waired/v1/inference/enable", m.handleInferenceEnable)
	mux.HandleFunc("/waired/v1/inference/disable", m.handleInferenceDisable)
	mux.HandleFunc("/waired/v1/inference/mesh", m.handleInferenceMesh)
	mux.HandleFunc("/waired/v1/worker", m.handleWorker)
	mux.HandleFunc("/waired/v1/inference/residency", m.handleResidency)
	mux.HandleFunc("/waired/v1/inference/model/unload", m.handleModelUnload)
	mux.HandleFunc("/waired/v1/integration/claude", m.handleClaudeIntegration)
	mux.HandleFunc("/waired/v1/integration/claude/route", m.handleClaudeRouting)
	mux.HandleFunc("/_mock/state", m.handleSetState)
	return mux
}

func routingNames() []string {
	out := make([]string, 0, len(routingScenarios))
	for n := range routingScenarios {
		out = append(out, string(n))
	}
	sort.Strings(out)
	return out
}

func (m *mockServer) state() connState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.s
}

func (m *mockServer) setState(s connState) {
	m.mu.Lock()
	m.s = s
	m.mu.Unlock()
}

// routingView is a consistent read of the routing axis, taken under one
// lock so a handler that renders both the worker response and the mesh
// snapshot cannot see them disagree — and so no builder needs the lock
// itself (which would deadlock a nested call under RLock).
type routingView struct {
	sc   routingScenario
	mode routingMode
	pin  string
}

func (m *mockServer) routingView() routingView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return routingView{sc: routingScenarios[m.routing], mode: m.workerMode, pin: m.workerPin}
}

func (m *mockServer) setRouting(n routingName, sc routingScenario) {
	m.mu.Lock()
	m.routing = n
	m.workerMode = sc.mode
	m.workerPin = sc.pin
	m.mu.Unlock()
}

func (m *mockServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s := m.state()
	resp := map[string]any{
		"network_id":    "net-mock-tray",
		"device_id":     selfDeviceID,
		"device_name":   selfDeviceName,
		"overlay_ip":    "100.64.0.7",
		"listen_port":   41820,
		"peer_count":    3,
		"disco_enabled": true,
		"observed_addr": "203.0.113.42:41820",
		"phase":         "active",
		"desired_phase": "active",
	}
	switch s {
	case stateConnected:
		// defaults above are the connected state
	case stateDisconnected:
		resp["peer_count"] = 0
		resp["overlay_ip"] = ""
		resp["observed_addr"] = ""
	case statePaused:
		resp["phase"] = "paused"
		resp["desired_phase"] = "paused"
	case stateError:
		// Simulate a daemon that is reachable but returning HTTP 500.
		http.Error(w, "simulated daemon error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (m *mockServer) handleIdentity(w http.ResponseWriter, r *http.Request) {
	s := m.state()
	if s == stateDisconnected || s == stateError {
		writeJSON(w, map[string]any{"enrolled": false})
		return
	}
	writeJSON(w, map[string]any{
		"enrolled":      true,
		"account_email": "alice@example.com",
		"network_name":  "mock-net",
		"network_id":    "net-mock-tray",
		"device_id":     selfDeviceID,
		"device_name":   selfDeviceName,
		"overlay_ip":    "100.64.0.7",
		"control_url":   "https://control.mock.example",
	})
}

func (m *mockServer) handlePause(w http.ResponseWriter, r *http.Request) {
	m.setState(statePaused)
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockServer) handleResume(w http.ResponseWriter, r *http.Request) {
	m.setState(stateConnected)
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockServer) handleInferenceStatus(w http.ResponseWriter, r *http.Request) {
	// Minimal InferenceStatus so the inference group renders.
	resp := map[string]any{
		"state":         "enabled",
		"desired_state": "enabled",
		"current_engine": map[string]any{
			"engine_id":   "ollama",
			"model_id":    "qwen2.5-coder-7b-instruct",
			"engine_name": "Ollama (Metal)",
		},
	}
	// The tray reads the routing mode off this hot path rather than polling
	// /waired/v1/worker (see management.InferenceStatus.Worker); a missing
	// key is exactly how a pre-worker daemon looks, which is what the
	// worker-off / off scenarios reproduce.
	if wr := workerResponse(m.routingView()); wr != nil {
		resp["worker"] = wr
	}
	m.mu.RLock()
	idle, resident := m.idle, m.resident
	m.mu.RUnlock()
	resp["residency"] = management.ResidencyResponse{
		IdleTimeout: idle.String(), HoldsIndefinitely: idle <= 0,
	}
	// The residency observation rides the runtime row, which is where the
	// tray reads it for the "(loaded)" suffix and the unload row.
	resp["runtimes"] = map[string]any{
		"ollama": management.RuntimeStatus{Installed: true, State: "ready", ModelResident: &resident},
	}
	if m.engine == engineLatchedStopped {
		resp["subsystem_state"] = "engine_failed"
		delete(resp, "current_engine")
		resp["runtimes"] = map[string]any{
			"ollama": management.RuntimeStatus{Name: "ollama", Installed: true, State: "not_started"},
			"vllm": management.RuntimeStatus{
				Name: "vllm", Installed: true, State: "stopped",
				FailureLatched: true,
				LastError: "engine repeatedly crashed; not retrying — another program is " +
					"already listening on 127.0.0.1:9479, the port the inference engine was " +
					"told to use — set inference.vllm_port in agent.json to a free port",
			},
		}
	}
	writeJSON(w, resp)
}

// handleResidency mirrors the daemon's residency endpoint so the preset
// rows move when clicked (waired-agent#861).
func (m *mockServer) handleResidency(w http.ResponseWriter, r *http.Request) {
	// Empty on a GET: reading the setting makes no claim about how it got
	// there.
	var effect management.ResidencyEffect
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req management.ResidencyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONStatus(w, http.StatusBadRequest,
				errorBody("bad_request", "invalid JSON: "+err.Error()))
			return
		}
		idle, err := management.ParseResidency(req.IdleTimeout)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, errorBody("bad_request", err.Error()))
			return
		}
		m.mu.Lock()
		m.idle = idle
		// Mirror the daemon's two branches so a surface built against
		// this mock sees both (waired-agent#908): a resident model is
		// re-stamped live, an empty engine has to be re-spawned for the
		// value to reach the next load.
		if m.resident {
			effect = management.ResidencyEffectLive
		} else {
			effect = management.ResidencyEffectEngineRestarted
		}
		m.mu.Unlock()
	default:
		writeJSONStatus(w, http.StatusMethodNotAllowed,
			errorBody("method_not_allowed", "GET or POST only"))
		return
	}
	m.mu.RLock()
	idle := m.idle
	m.mu.RUnlock()
	writeJSON(w, management.ResidencyResponse{
		IdleTimeout: idle.String(), HoldsIndefinitely: idle <= 0, Effect: effect,
	})
}

// handleModelUnload flips the residency observation so the tray's
// "(loaded)" suffix and the unload row's own state visibly follow the
// click, rather than reporting a success nothing shows.
func (m *mockServer) handleModelUnload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONStatus(w, http.StatusMethodNotAllowed,
			errorBody("method_not_allowed", "POST only"))
		return
	}
	m.mu.Lock()
	was := m.resident
	m.resident = false
	m.mu.Unlock()
	resp := management.ModelUnloadResponse{Unloaded: was}
	if was {
		resp.Model = "qwen2.5-coder-7b-instruct"
	}
	writeJSON(w, resp)
}

func (m *mockServer) handleInferenceEnable(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleInferenceMesh serves the peers-only mesh aggregate the tray turns
// into the "Mesh: …" row and the pin rows. Mirrors the real handler's
// method check; 404 under the mesh-off / off scenarios so the tray takes
// its pre-mesh-API path (Client.MeshSnapshot folds 404 into a nil snapshot).
func (m *mockServer) handleInferenceMesh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rv := m.routingView()
	if rv.sc.meshOff {
		http.Error(w, "inference mesh provider not attached", http.StatusNotFound)
		return
	}
	writeJSON(w, meshSnapshot(rv))
}

// handleWorker serves the operator's manual routing choice. POST is what a
// mode row, a pin row and "(clear pin)" all land on, so it applies the same
// validation the daemon does (Server.applyWorkerRequest) — a mock that
// accepted everything would make a tray-side bug indistinguishable from a
// fixture-side one.
func (m *mockServer) handleWorker(w http.ResponseWriter, r *http.Request) {
	if m.routingView().sc.workerOff {
		writeJSONStatus(w, http.StatusNotFound,
			errorBody("not_found", "worker control not configured"))
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var req management.WorkerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONStatus(w, http.StatusBadRequest,
				errorBody("bad_request", "invalid JSON: "+err.Error()))
			return
		}
		if err := m.applyWorker(req); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, errorBody("bad_request", err.Error()))
			return
		}
	default:
		writeJSONStatus(w, http.StatusMethodNotAllowed,
			errorBody("method_not_allowed", "GET or POST only"))
		return
	}
	writeJSON(w, workerResponse(m.routingView()))
}

// applyWorker mirrors management.Server.applyWorkerRequest: every non-pinned
// mode rejects a pin, pinned requires one, and anything else is a 400.
func (m *mockServer) applyWorker(req management.WorkerRequest) error {
	switch req.Mode {
	case "", state.RoutingModeAuto, state.RoutingModeLocalOnly,
		state.RoutingModePeerPreferred, state.RoutingModePeerOnly:
		if req.PinnedPeerDeviceID != "" {
			name := string(req.Mode)
			if name == "" {
				name = string(state.RoutingModeAuto)
			}
			return errors.New(name + " mode must not carry pinned_peer_device_id")
		}
		mode := req.Mode
		if mode == "" {
			mode = state.RoutingModeAuto
		}
		m.setWorker(mode, "")
	case state.RoutingModePinned:
		if req.PinnedPeerDeviceID == "" {
			return errors.New("pinned mode requires pinned_peer_device_id")
		}
		m.setWorker(state.RoutingModePinned, req.PinnedPeerDeviceID)
	default:
		return errors.New("unknown mode " + string(req.Mode))
	}
	return nil
}

func (m *mockServer) setWorker(mode routingMode, pin string) {
	m.mu.Lock()
	m.workerMode = mode
	m.workerPin = pin
	m.mu.Unlock()
}

// workerResponse renders the current choice the way the daemon does,
// including the derived pin name/status the tray's summary row needs.
// nil means "this daemon has no worker control" (the worker-off / off
// scenarios), which is how the key is dropped from /inference/status.
func workerResponse(rv routingView) *management.WorkerResponse {
	if rv.sc.workerOff {
		return nil
	}
	resp := &management.WorkerResponse{Mode: rv.mode, PinnedPeerDeviceID: rv.pin}
	if rv.mode == state.RoutingModePinned && rv.pin != "" {
		if rv.sc.meshOff {
			// Same as the daemon with no aggregator wired: it cannot tell a
			// gone peer from an unknown one, so everything reads "absent".
			resp.PinnedPeerStatus = "absent"
		} else {
			var display string
			resp.PinnedPeerName, resp.PinnedPeerStatus, display = pinStatus(meshSnapshot(rv), rv.pin)
			resp.PinnedPeerDisplayID = display
		}
	}
	return resp
}

// pinStatus mirrors management.Server.resolvePinStatus so the tray's
// "(unavailable)" / "(absent)" suffixes appear for the same reasons they
// would in production — including the display identifier, which is the
// grant pseudonym for a public machine (#739).
func pinStatus(snap inferencemesh.Snapshot, deviceID string) (name, status, display string) {
	for _, p := range snap.Peers {
		if p.DeviceID != deviceID {
			continue
		}
		switch {
		case p.Stale:
			status = "unavailable"
		case p.InferenceState == nil || !p.InferenceState.Reachable:
			status = "unavailable"
		case len(p.InferenceState.Models) == 0:
			status = "unavailable"
		default:
			status = "ok"
		}
		display, _ = inferencemesh.PeerDisplayID(p)
		return p.DeviceName, status, display
	}
	return "", "absent", ""
}

// meshSnapshot builds the fake aggregate. Peers come out in device-name
// order because that is the contract the real aggregator now holds
// (waired-agent#326) and the thing that keeps a peer on the same menu row
// poll over poll — a mock that shuffled would re-introduce the very bug
// the tray was fixed for.
func meshSnapshot(rv routingView) inferencemesh.Snapshot {
	now := time.Now()
	fresh := now.Format(time.RFC3339Nano)
	snap := inferencemesh.Snapshot{
		GeneratedAt:          fresh,
		SelfDeviceID:         selfDeviceID,
		StalenessThresholdMS: 15_000,
		FrameStalenessMS:     30_000,
		MapReceivedAt:        now.Add(-2 * time.Second).Format(time.RFC3339Nano),
		MapAgeMS:             2_000,
		Self: inferencemesh.PeerView{
			DeviceID:   selfDeviceID,
			DeviceName: selfDeviceName,
			OverlayIP:  "100.64.0.7",
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Endpoint:  "http://127.0.0.1:11434",
				Models:    []string{"qwen2.5-coder-7b-instruct"},
				LastCheck: fresh,
			},
		},
		Peers: []inferencemesh.PeerView{},
	}
	if rv.sc.noPeers {
		return snap
	}
	for _, p := range meshPeers {
		pv := inferencemesh.PeerView{
			DeviceID:   p.deviceID,
			DeviceName: p.deviceName,
			OverlayIP:  p.overlayIP,
			Stale:      p.stale || rv.sc.peersDown,
		}
		if p.engine != "" {
			last := fresh
			if pv.Stale {
				last = now.Add(-90 * time.Second).Format(time.RFC3339Nano)
			}
			pv.InferenceState = &signer.InferenceState{
				Reachable: true,
				Type:      p.engine,
				Endpoint:  "http://127.0.0.1:11434",
				Models:    p.models,
				LastCheck: last,
			}
		}
		snap.Peers = append(snap.Peers, pv)
	}
	// Reachable is the peers-only OR (self never contributes), computed the
	// same way the tray decides a peer is usable.
	for _, p := range snap.Peers {
		if !p.Stale && p.InferenceState != nil && p.InferenceState.Reachable &&
			len(p.InferenceState.Models) > 0 {
			snap.Reachable = true
			break
		}
	}
	return snap
}

// handleClaudeIntegration reports Claude Code as routed through Waired so the
// "Claude integration: ● active" header + managed-settings row render (and the
// routing submenu's enable-note stays hidden).
func (m *mockServer) handleClaudeIntegration(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"wrapper":     map[string]any{"reachable": true},
		"binary_path": "/usr/bin/waired",
		"managed_settings": map[string]any{
			"supported":         true,
			"present":           true,
			"base_url":          "http://127.0.0.1:9472",
			"expected_base_url": "http://127.0.0.1:9472",
			"configured":        true,
		},
	})
}

// handleClaudeRouting serves the per-class routing policy (#649). POST mutates
// the in-memory policy (nil field = unchanged) so clicking a route in the tray
// moves the ●/○ on the next poll. A sample last-fallback is attached while the
// main route is "auto" so the fallback note row is exercised too.
func (m *mockServer) handleClaudeRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Main *string `json:"main"`
			Sub  *string `json:"sub"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		if req.Main != nil {
			m.claudeMain = *req.Main
		}
		if req.Sub != nil {
			m.claudeSub = *req.Sub
		}
		m.mu.Unlock()
	}
	m.mu.RLock()
	main, sub := m.claudeMain, m.claudeSub
	m.mu.RUnlock()
	resp := map[string]any{
		"policy": map[string]any{"main": main, "sub": sub},
	}
	if main == "auto" {
		resp["last_fallback"] = map[string]any{
			"when":      time.Now().Add(-30 * time.Second).Format(time.RFC3339),
			"class":     "main",
			"reason":    "local_status_503",
			"direction": "anthropic",
			"count":     1,
		}
	}
	writeJSON(w, resp)
}

func (m *mockServer) handleInferenceDisable(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleSetState lets the operator flip either axis without restarting:
//
//	curl -XPOST 'http://127.0.0.1:9476/_mock/state?value=paused'
//	curl -XPOST 'http://127.0.0.1:9476/_mock/state?routing=pinned-unavailable'
//
// Both may be given at once. Flipping the routing scenario resets the worker
// mode/pin to that scenario's starting point, discarding clicks made since.
func (m *mockServer) handleSetState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	v, hasValue := q["value"]
	rt, hasRouting := q["routing"]
	if !hasValue && !hasRouting {
		http.Error(w, "want ?value=<state> and/or ?routing=<scenario>", http.StatusBadRequest)
		return
	}

	var applied []string
	if hasValue {
		switch connState(v[0]) {
		case stateConnected, stateDisconnected, statePaused, stateError:
			m.setState(connState(v[0]))
			applied = append(applied, "state="+v[0])
		default:
			http.Error(w, "unknown state: "+v[0], http.StatusBadRequest)
			return
		}
	}
	if hasRouting {
		sc, ok := routingScenarios[routingName(rt[0])]
		if !ok {
			http.Error(w, "unknown routing scenario: "+rt[0]+
				" (want one of: "+strings.Join(routingNames(), ", ")+")", http.StatusBadRequest)
			return
		}
		m.setRouting(routingName(rt[0]), sc)
		applied = append(applied, "routing="+rt[0]+" ("+sc.descr+")")
	}
	for _, a := range applied {
		fmt.Fprintf(os.Stderr, "mock-mgmt: %s\n", a)
		_, _ = fmt.Fprintf(w, "%s\n", a)
	}
}

// errorBody mirrors management.errorBody's wire shape: the tray surfaces the
// raw response body in its error dialog, so a mock 400 should read like a
// daemon 400.
func errorBody(code, msg string) map[string]string {
	return map[string]string{"error_code": code, "message": msg}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
