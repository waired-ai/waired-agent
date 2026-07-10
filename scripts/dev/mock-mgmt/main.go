// Command mock-mgmt stands in for the agent's Local Management API
// for the limited purpose of driving the desktop tray during darwin
// UI screenshot verification.
//
// It is NOT a fixture for unit tests (the tray package has its own
// httptest-based tests); the point of this binary is to give the
// human running `make build-tray-darwin` a real HTTP server they
// can swap into a particular fake state and screenshot the tray's
// rendering against.
//
// Usage:
//
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state connected
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state disconnected
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state paused
//	go run ./scripts/dev/mock-mgmt -listen 127.0.0.1:9476 -state error
//
// While running, the state can be flipped without restart via:
//
//	curl -XPOST 127.0.0.1:9476/_mock/state?value=disconnected
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type state string

const (
	stateConnected    state = "connected"
	stateDisconnected state = "disconnected"
	statePaused       state = "paused"
	stateError        state = "error"
)

// mockServer holds the current fake state and serves the subset of
// /waired/v1/* endpoints the tray polls. Other endpoints return 404
// so the tray's Err*Unsupported fall-throughs fire and the relevant
// menu groups (Claude / OpenCode / inference catalog) hide.
type mockServer struct {
	mu sync.RWMutex
	s  state
	// Claude Code per-class routing policy (#649/#650) — mutated in place
	// by POST /waired/v1/integration/claude/route so the tray's ●/○ marks
	// move when the operator clicks.
	claudeMain string
	claudeSub  string
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9476",
		"address to bind (default matches management.DefaultListen)")
	initial := flag.String("state", "connected",
		"initial state: connected | disconnected | paused | error")
	flag.Parse()

	srv := &mockServer{s: state(*initial), claudeMain: "auto", claudeSub: "same"}

	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/status", srv.handleStatus)
	mux.HandleFunc("/waired/v1/identity", srv.handleIdentity)
	mux.HandleFunc("/waired/v1/pause", srv.handlePause)
	mux.HandleFunc("/waired/v1/resume", srv.handleResume)
	mux.HandleFunc("/waired/v1/inference/status", srv.handleInferenceStatus)
	mux.HandleFunc("/waired/v1/inference/enable", srv.handleInferenceEnable)
	mux.HandleFunc("/waired/v1/inference/disable", srv.handleInferenceDisable)
	mux.HandleFunc("/waired/v1/integration/claude", srv.handleClaudeIntegration)
	mux.HandleFunc("/waired/v1/integration/claude/route", srv.handleClaudeRouting)
	mux.HandleFunc("/_mock/state", srv.handleSetState)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "mock-mgmt listening on %s (state=%s)\n", *listen, srv.s)
	log.Fatal(httpSrv.ListenAndServe())
}

func (m *mockServer) state() state {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.s
}

func (m *mockServer) setState(s state) {
	m.mu.Lock()
	m.s = s
	m.mu.Unlock()
}

func (m *mockServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s := m.state()
	resp := map[string]any{
		"network_id":    "net-mock-tray",
		"device_id":     "dev-mock-laptop",
		"device_name":   "mock-laptop",
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
		"device_id":     "dev-mock-laptop",
		"device_name":   "mock-laptop",
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
	writeJSON(w, map[string]any{
		"state":         "enabled",
		"desired_state": "enabled",
		"current_engine": map[string]any{
			"engine_id":   "ollama",
			"model_id":    "qwen2.5-coder-7b-instruct",
			"engine_name": "Ollama (Metal)",
		},
	})
}

func (m *mockServer) handleInferenceEnable(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
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

// handleSetState lets the operator flip the mock state without
// restarting:  curl -XPOST 'http://127.0.0.1:9476/_mock/state?value=paused'
func (m *mockServer) handleSetState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	v := r.URL.Query().Get("value")
	switch state(v) {
	case stateConnected, stateDisconnected, statePaused, stateError:
		m.setState(state(v))
		fmt.Fprintf(os.Stderr, "mock-mgmt: state -> %s\n", v)
		_, _ = fmt.Fprintf(w, "state=%s\n", v)
	default:
		http.Error(w, "unknown state: "+v, http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
