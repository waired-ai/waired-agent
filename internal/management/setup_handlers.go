package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Setup executor phases (waired#835 §9/§11). The executor is the still-
// running elevated CLI from `sudo waired init`; it reports what it is
// doing so the daemon can turn a dead executor into an honest
// executor_gone step instead of a spinner that never resolves.
const (
	SetupExecutorPhaseIdle       = "idle"
	SetupExecutorPhaseInstalling = "installing"
	SetupExecutorPhaseDone       = "done"
	SetupExecutorPhaseFailed     = "failed"
)

// validSetupExecutorPhase reports whether p is one of the four phases.
// An empty phase is accepted and treated as idle so a bare attach POST
// does not have to spell it out.
func validSetupExecutorPhase(p string) bool {
	switch p {
	case "", SetupExecutorPhaseIdle, SetupExecutorPhaseInstalling,
		SetupExecutorPhaseDone, SetupExecutorPhaseFailed:
		return true
	}
	return false
}

// SetupStateResponse is the body of GET /waired/v1/setup/state — what a
// setup executor needs to decide whether to act, entirely derived from
// observable daemon state (waired#835 §6: no second source of truth).
type SetupStateResponse struct {
	// Active is true once a desired-state instruction has been seen on
	// this device's own map entry, i.e. the operator has actually
	// started setup in the browser.
	Active bool `json:"active"`
	// The desired triple the control plane is currently serving.
	DesiredEngine       string `json:"desired_engine,omitempty"`
	DesiredModelID      string `json:"desired_model_id,omitempty"`
	DesiredBenchmarkGen int    `json:"desired_benchmark_gen,omitempty"`
	// EngineInstalled / EngineReady describe the desired engine on this
	// host; both false when no engine is desired.
	EngineInstalled bool `json:"engine_installed"`
	EngineReady     bool `json:"engine_ready"`
	// ExecutorAttached is true while a lease is live.
	ExecutorAttached bool `json:"executor_attached"`
	// ExecutorElevated echoes the live lease's self-asserted elevation.
	ExecutorElevated bool `json:"executor_elevated"`
	// InstallClaimed names the engine whose installation a live lease has
	// claimed; empty means a fresh executor may claim it. The claim is
	// bound to the LEASE, not to desired_engine — it clears when the
	// claiming lease expires or is released without phase=done, so the
	// "re-run sudo waired init" recovery path actually recovers
	// (waired#835 §11.1).
	InstallClaimed string `json:"install_claimed,omitempty"`
	// StateDir is the daemon's own state directory — where an executor
	// must put a bundled engine so this daemon can find it again. The
	// daemon declares it and the executor obeys rather than recomputing
	// it, because a CLI-side defaultStateDir() silently diverges from a
	// daemon started with --state-dir or $WAIRED_STATE_DIR, and the
	// symptom of divergence is silent: the install succeeds, the daemon
	// looks elsewhere, and engine_install spins forever (waired#835
	// §11.1). Empty before enrollment or with inference off, which the
	// executor must read as "do not install" — never as "guess".
	//
	// This does not weaken §17.1's no-paths-on-the-wire rule: that rule
	// governs values crossing the control-plane trust boundary, and this
	// is a daemon's own value returned to a co-local process that could
	// already compute it.
	StateDir string `json:"state_dir,omitempty"`
}

// SetupExecutorRequest is the body of POST /waired/v1/setup/executor:
// one lease heartbeat from the elevated CLI.
//
// Trust model (waired#835 §11.1): Attached and Elevated are SELF-ASSERTED
// over an API that is unauthenticated to local processes by design (the
// local IPC socket, #838 — writeGuard forces the transport, it does not
// identify the caller). The lease is a liveness hint from a co-local,
// already-trusted process; it never grants privilege, and every actual
// engine install is performed by the elevated CLI itself. A local process
// that lies here can suppress the honest permission_denied copy, which is
// the same blast radius as the existing unauthenticated local writes
// (/waired/v1/inference/enable, /waired/v1/public/share/enable).
type SetupExecutorRequest struct {
	// Attached false releases the lease; true attaches or renews it.
	Attached bool `json:"attached"`
	// Elevated is false when the CLI is not running with the privileges
	// an engine install needs — the daemon then keeps reporting
	// permission_denied rather than a misleading executor_gone.
	Elevated bool `json:"elevated"`
	// Phase is idle | installing | done | failed (empty = idle).
	Phase string `json:"phase"`
	// Engine names the engine an installing/done/failed phase refers to.
	Engine string `json:"engine,omitempty"`
	// Error carries the install failure detail for phase=failed.
	Error string `json:"error,omitempty"`
}

// SetupExecutorController is implemented by the agent's desired-state
// reconciler (cmd/waired-agent). Kept narrow so this package never
// imports the binary.
type SetupExecutorController interface {
	// SetupState projects the reconciler's current view for an executor.
	SetupState(ctx context.Context) SetupStateResponse
	// NoteExecutor records one lease heartbeat (or release) and returns
	// the resulting state, so an executor learns the install claim in the
	// same round trip.
	NoteExecutor(ctx context.Context, req SetupExecutorRequest) SetupStateResponse
}

// WithSetupExecutor attaches a SetupExecutorController so the server
// exposes GET /waired/v1/setup/state and POST /waired/v1/setup/executor —
// the agent-local executor lease (waired#835 §9/§11). Passing nil leaves
// both routes unregistered, which is what an older CLI probes for.
// Returns the receiver for chaining.
func (s *Server) WithSetupExecutor(c SetupExecutorController) *Server {
	s.setupExecutor = c
	return s
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.setupExecutor.SetupState(r.Context()))
}

func (s *Server) handleSetupExecutor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req SetupExecutorRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read body"})
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	if !validSetupExecutorPhase(req.Phase) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid phase"})
		return
	}
	writeJSON(w, http.StatusOK, s.setupExecutor.NoteExecutor(r.Context(), req))
}
