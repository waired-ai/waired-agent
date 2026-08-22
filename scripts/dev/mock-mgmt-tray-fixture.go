//go:build ignore

// Command scripts/dev/mock-mgmt-tray-fixture serves a curated subset of
// the Local Management API for visually verifying the Linux tray
// without bringing up the real daemon (WireGuard / Control Plane /
// GCE testnet).
//
// Endpoints (GET-only stubs except where noted):
//
//	GET  /waired/v1/status              — fixed enrolled view; phase
//	                                      toggles via SIGUSR1
//	GET  /waired/v1/identity            — fixed enrolled view
//	GET  /waired/v1/inference/status    — synthetic "ready" engine
//	GET  /waired/v1/inference/catalog   — 4-family fixture
//	GET  /waired/v1/integration/claude  — wrapper reachable + proxy enabled
//	GET  /waired/v1/integration/opencode— configured at fake config path
//	GET  /waired/v1/observability/state — time-driven AgentState
//	                                      (engine_ready flips off at t=12min)
//	GET  /waired/v1/observability/events— kind=fallback events injected
//	                                      at t=10s and t=30s; honours
//	                                      since= / kinds= / limit=
//	POST /waired/v1/pause               — flips phase to paused
//	POST /waired/v1/resume              — flips phase to active
//
// Visual-verification scenario timeline (default scenario):
//
//	t=0      — Connected idle
//	t=10s    — first fallback (engine_not_ready)  → tray icon Degraded,
//	                                                  submenu 1 row
//	t=30s    — second fallback (capacity_full)    → submenu 2 rows
//	t=11min  — both events past 10-min cutoff     → tray icon Connected,
//	                                                  submenu hidden
//	t=12min  — engine_ready flips false           → submenu stays hidden
//	                                                  (engine_ready alone
//	                                                  does not promote)
//
// Send SIGUSR1 to flip phase paused/active manually (legacy Phase 1-2
// UX check).
//
// Run:
//
//	go run scripts/dev/mock-mgmt-tray-fixture.go            # :9476, default scenario
//	go run scripts/dev/mock-mgmt-tray-fixture.go -addr :18080 -scenario idle
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

var (
	paused   atomic.Bool
	scenario *scenarioState
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9476", "listen address")
	socket := flag.String("socket", "",
		"also serve the same mux on this unix-domain socket path; export WAIRED_MGMT_SOCKET=<path> so the tray sends its writes here (waired#838)")
	scenarioName := flag.String("scenario", "default",
		"scenario: default (Phase 8.5 timeline) or idle (no fallbacks)")
	flag.Parse()

	scenario = newScenario(*scenarioName)
	scenario.start = time.Now()

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGUSR1)
		for range ch {
			paused.Store(!paused.Load())
			fmt.Fprintf(os.Stderr, "mock-mgmt: paused=%v\n", paused.Load())
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", handleStatus)
	mux.HandleFunc("/waired/v1/identity", handleIdentity)
	mux.HandleFunc("/waired/v1/inference/status", handleInferenceStatus)
	mux.HandleFunc("/waired/v1/inference/catalog", handleInferenceCatalog)
	mux.HandleFunc("/waired/v1/integration/claude", handleClaudeIntegration)
	mux.HandleFunc("/waired/v1/integration/opencode", handleOpenCodeIntegration)
	mux.HandleFunc("/waired/v1/observability/state", handleObservabilityState)
	mux.HandleFunc("/waired/v1/observability/events", handleObservabilityEvents)
	mux.HandleFunc("/waired/v1/pause", func(w http.ResponseWriter, _ *http.Request) {
		paused.Store(true)
		fmt.Fprintln(os.Stderr, "mock-mgmt: pause via API")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/waired/v1/resume", func(w http.ResponseWriter, _ *http.Request) {
		paused.Store(false)
		fmt.Fprintln(os.Stderr, "mock-mgmt: resume via API")
		w.WriteHeader(http.StatusNoContent)
	})

	fmt.Fprintf(os.Stderr, "mock-mgmt listening on %s (scenario=%s)\n", *addr, *scenarioName)
	if len(scenario.plan) > 0 {
		fmt.Fprintln(os.Stderr, "timeline:")
		for _, e := range scenario.plan {
			fmt.Fprintf(os.Stderr, "  t=%-9s %s\n", e.at.Truncate(time.Second), e.descr)
		}
	}
	// Since waired#838 the tray sends MUTATING requests (pause/resume/...)
	// over a local IPC socket rather than the loopback TCP port, so serve
	// the same mux there too when asked; point the tray at it with
	// WAIRED_MGMT_SOCKET=<path>.
	if *socket != "" {
		_ = os.Remove(*socket)
		ln, lerr := net.Listen("unix", *socket)
		if lerr != nil {
			fmt.Fprintln(os.Stderr, lerr)
			os.Exit(1)
		}
		if cerr := os.Chmod(*socket, 0o666); cerr != nil {
			fmt.Fprintln(os.Stderr, cerr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "mock-mgmt also serving writes on unix %s (export WAIRED_MGMT_SOCKET=%s)\n", *socket, *socket)
		go func() {
			if serr := http.Serve(ln, mux); serr != nil {
				fmt.Fprintln(os.Stderr, serr)
			}
		}()
	}

	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// --- scenario state machine ---

type scenarioState struct {
	name  string
	mu    sync.Mutex
	start time.Time

	nextSeq uint64
	ring    []observability.Event

	injected map[string]bool
	plan     []scheduledEvent
}

type scheduledEvent struct {
	at    time.Duration
	descr string
	apply func(*scenarioState)
}

func newScenario(name string) *scenarioState {
	s := &scenarioState{name: name, injected: map[string]bool{}}
	switch name {
	case "idle":
		// No fallbacks; engine stays ready forever.
	default:
		s.plan = []scheduledEvent{
			{
				at:    10 * time.Second,
				descr: "inject fallback #1 (engine_not_ready)",
				apply: func(s *scenarioState) {
					s.injectFallback("dev_local_a", "peer_b", "engine_not_ready", "qwen3:8b")
				},
			},
			{
				at:    30 * time.Second,
				descr: "inject fallback #2 (capacity_full)",
				apply: func(s *scenarioState) {
					s.injectFallback("dev_local_a", "peer_c", "capacity_full", "qwen3:8b")
				},
			},
			{
				at:    11 * time.Minute,
				descr: "(events now past 10-min cutoff; tray returns to Connected)",
				apply: func(*scenarioState) {},
			},
			{
				at:    12 * time.Minute,
				descr: "flip engine_ready=false",
				apply: func(*scenarioState) {},
			},
		}
	}
	return s
}

// advance fires planned events whose scheduled time has elapsed.
// Idempotent: each scheduled event is fired exactly once via the
// `injected[descr]` flag.
func (s *scenarioState) advance() {
	s.mu.Lock()
	defer s.mu.Unlock()
	elapsed := time.Since(s.start)
	for _, e := range s.plan {
		if elapsed >= e.at && !s.injected[e.descr] {
			e.apply(s)
			s.injected[e.descr] = true
			fmt.Fprintf(os.Stderr, "mock-mgmt: t=%v %s\n",
				elapsed.Truncate(time.Second), e.descr)
		}
	}
}

func (s *scenarioState) injectFallback(from, to, reason, model string) {
	s.nextSeq++
	s.ring = append(s.ring, observability.Event{
		Seq:  s.nextSeq,
		TS:   time.Now(),
		Kind: observability.KindFallback,
		Fallback: &observability.FallbackEvent{
			From: from, To: to, Reason: reason, Model: model,
		},
	})
}

func (s *scenarioState) snapshotState() management.ObservabilityState {
	s.mu.Lock()
	elapsed := time.Since(s.start)
	s.mu.Unlock()

	engineReady := true
	if s.name == "default" && elapsed >= 12*time.Minute {
		engineReady = false
	}
	return management.ObservabilityState{
		Agent: management.AgentState{
			DeviceID:      "dev_local_a",
			Version:       "0.0.0-mock",
			UptimeSeconds: int64(elapsed.Seconds()),
			EngineReady:   engineReady,
			ModelID:       "qwen3:8b",
			ShareEnabled:  true,
			Paused:        paused.Load(),
			CapacityTotal: 10,
			CapacityUsed:  2,
			Inflight:      0,
		},
		Mesh: management.MeshState{
			PeersEnrolled:  3,
			PeersReachable: 2,
			PeersReady:     2,
		},
	}
}

func (s *scenarioState) snapshotEvents(since uint64, kinds []observability.Kind, limit int) observabilityclient.EventsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]observability.Event, 0, len(s.ring))
	var oldestSeq uint64
	if len(s.ring) > 0 {
		oldestSeq = s.ring[0].Seq
	}
	for _, ev := range s.ring {
		if ev.Seq <= since {
			continue
		}
		if len(kinds) > 0 && !slices.Contains(kinds, ev.Kind) {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	resp := observabilityclient.EventsResponse{
		Events:    out,
		OldestSeq: oldestSeq,
	}
	if len(out) > 0 {
		resp.NextSince = out[len(out)-1].Seq
	} else {
		resp.NextSince = since
	}
	return resp
}

// --- handlers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	phase := "active"
	if paused.Load() {
		phase = "paused"
	}
	writeJSON(w, http.StatusOK, management.Status{
		NetworkID:    "net_alice",
		DeviceID:     "dev_local_a",
		DeviceName:   "alice-sv-mag",
		OverlayIP:    "100.96.0.10",
		ListenPort:   51820,
		PeerCount:    3,
		DiscoEnabled: true,
		Phase:        phase,
		DesiredPhase: phase,
	})
}

func handleIdentity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, management.IdentityView{
		Enrolled:     true,
		AccountEmail: "alice@example.com",
		NetworkName:  "alice-net",
		NetworkID:    "net_alice",
		DeviceID:     "dev_local_a",
		DeviceName:   "alice-sv-mag",
		OverlayIP:    "100.96.0.10",
		ControlURL:   "https://control.example.com",
	})
}

func handleInferenceStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, management.InferenceStatus{
		SubsystemState: "ready",
		Runtimes: map[string]management.RuntimeStatus{
			"ollama": {Name: "ollama", Installed: true, Version: "0.5.1", State: "ready"},
		},
		Models: management.ModelsSnapshot{
			Ready: []string{"qwen3:8b"},
		},
		ActiveEndpoints: []management.ActiveEndpoint{
			{EndpointID: "ollama-1", Runtime: "ollama", ModelID: "qwen3:8b", State: "ready"},
		},
		Active: &management.ActiveSelection{
			Runtime:   "ollama",
			ModelID:   "qwen3:8b",
			VariantID: "q4_K_M",
			DecidedBy: "user",
		},
		DesiredState:  "enabled",
		ShareWithMesh: "shared",
	})
}

func handleInferenceCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, management.ModelCatalogResponse{
		PreferredModelID: "qwen3:8b",
		Engine:           "ollama",
		Host: management.CatalogHost{
			RAMTotalGB:  32,
			VRAMTotalMB: 12288,
			GPUModel:    "NVIDIA RTX 4070",
		},
		Active: &management.CatalogActive{
			ModelID:     "qwen3:8b",
			VariantID:   "q4_K_M",
			DisplayName: "Qwen3 8B",
		},
		Families: []management.CatalogFamily{
			{
				ModelID: "qwen3:8b", DisplayName: "Qwen3 8B",
				BestFitVariantID: "q4_K_M", Fits: true,
				Active: true, Preferred: true, Downloaded: true,
			},
			{
				ModelID: "llama3:8b", DisplayName: "Llama 3 8B",
				BestFitVariantID: "q4_K_M", Fits: true,
			},
			{
				ModelID: "mistral:7b", DisplayName: "Mistral 7B",
				BestFitVariantID: "q4_K_M", Fits: true,
			},
			{
				ModelID: "phi3:medium", DisplayName: "Phi-3 Medium",
				Fits: false, DeficitLabel: "needs 28GB VRAM",
			},
		},
	})
}

func handleClaudeIntegration(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, management.ClaudeIntegrationStatus{
		Wrapper: management.ClaudeWrapperView{
			Reachable: true,
			State: &management.ClaudeIntegrationStateView{
				Phase:                   "ready",
				PID:                     12345,
				Updated:                 time.Now().UTC().Format(time.RFC3339),
				GatewayURL:              "http://127.0.0.1:9476",
				InferenceReachableLocal: true,
			},
		},
		BinaryPath: "/usr/local/bin/waired",
		// Transparent proxy enabled and fully live: redirect in the hosts
		// file and the MITM CA trusted.
		Proxy: management.ClaudeProxyView{
			Supported:      true,
			Desired:        "enabled",
			HostsRedirect:  true,
			CACertPresent:  true,
			InterceptHosts: []string{"api.anthropic.com"},
		},
	})
}

func handleOpenCodeIntegration(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, management.OpenCodeIntegrationStatus{
		Config: detect.Result{
			Path:         "/home/alice/.config/opencode/config.json",
			Configured:   true,
			Stale:        false,
			CurrentValue: "http://127.0.0.1:9476",
		},
	})
}

func handleObservabilityState(w http.ResponseWriter, _ *http.Request) {
	scenario.advance()
	writeJSON(w, http.StatusOK, scenario.snapshotState())
}

func handleObservabilityEvents(w http.ResponseWriter, r *http.Request) {
	scenario.advance()
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	var kinds []observability.Kind
	if raw := q.Get("kinds"); raw != "" {
		for k := range strings.SplitSeq(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds = append(kinds, observability.Kind(k))
			}
		}
	}
	writeJSON(w, http.StatusOK, scenario.snapshotEvents(since, kinds, limit))
}
