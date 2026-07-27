package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/proto/signer"
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

// Setup step ids the executor can report against (waired#835 §7, the
// five-id set settled in waired#934). The ids are free-form strings on
// the CP wire — this constant block is not a wire schema, it is the
// vocabulary the CLI and the daemon must share to talk about the same
// row, and it lives here because both `cmd/waired` and
// `cmd/waired-agent` import this package.
//
// Only the steps a privileged executor drives appear here. `model_pull`
// and `benchmark` are the daemon's own and never cross this API.
const (
	SetupStepEngineDownload = "engine_download"
	SetupStepEngineInstall  = "engine_install"
	SetupStepIntegration    = "integration"
)

// validSetupExecutorStep reports whether s names a step an executor may
// report. Empty is accepted and means engine_install — that is what a
// CLI predating the split reports, and it only ever meant the install.
func validSetupExecutorStep(s string) bool {
	switch s {
	case "", SetupStepEngineDownload, SetupStepEngineInstall, SetupStepIntegration:
		return true
	}
	return false
}

// IsEngineSetupStep reports whether a step id refers to the engine
// install the lease claims. The claim covers both halves of it — the
// download and the install proper — so a phase reported against either
// moves the same claim, while a step outside the engine (the coding-agent
// integration) must never touch it.
func IsEngineSetupStep(s string) bool {
	return s == "" || s == SetupStepEngineDownload || s == SetupStepEngineInstall
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
	// Integrations is the coding-agent instruction (waired#935), for the
	// elevated executor to apply — the daemon deliberately does not,
	// because writing into a user's home (and, for Claude Code,
	// root-owned managed settings) would make it a privilege bridge.
	//
	// A POINTER for its three states: nil means no instruction, an empty
	// slice means asked with every toggle off, and a populated one names
	// what to write. The middle state is what stops "nobody asked" from
	// reading as "asked and satisfied".
	//
	// Named without the `Desired` prefix its siblings carry, on purpose:
	// scripts/ci/protoconsumer matches field WRITES by name, so a local
	// `DesiredIntegrations` here would look like a producer for
	// signer.InferenceState.DesiredIntegrations — which the control plane
	// injects and this repo must never write — and would silently retire
	// the guard's cover for it.
	Integrations *[]string `json:"desired_integrations,omitempty"`
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

	// Step names which setup step (§7) this report is about; empty means
	// engine_install, which is what every executor predating the split
	// reported. The lease drives more than one step now — the engine
	// download, the install proper, and the coding-agent integration —
	// and the daemon keeps one phase per step rather than one per lease,
	// so a finished download is not overwritten by the install that
	// follows it (waired-agent#197).
	Step string `json:"step,omitempty"`
	// CompletedBytes / TotalBytes / RateBps are the live transfer figures
	// for a byte-denominated step. They exist because the engine install
	// runs in the elevated CLI and writes its progress to that terminal:
	// without them the browser wizard has nothing to show for a 1.4 GB
	// download and renders "Working on it…" for the whole install
	// (waired-agent#197).
	//
	// 0 / omitted means unknown, for the rate as much as for the counts.
	// A stall is derived from CompletedBytes not advancing, deliberately
	// NOT from RateBps == 0 — the CP wire collapses 0 into absent, so the
	// CLI renderer's "-1 = unknown / 0 = stalled" sentinel pair cannot
	// survive the trip and must not be reinvented here.
	CompletedBytes int64 `json:"completed_bytes,omitempty"`
	TotalBytes     int64 `json:"total_bytes,omitempty"`
	RateBps        int64 `json:"rate_bps,omitempty"`

	// Driver claims the setup for a surface — one of the
	// signer.SetupDriver* values (waired-agent#198). Only the terminal
	// ever sets it: the browser's claim is implicit in the desired state
	// it wrote, which the daemon can already see, while a terminal
	// takeover leaves no trace anywhere else.
	//
	// It is bound to the LEASE, like the install claim: the terminal is
	// only driving for as long as the process holding it is alive, and a
	// latch that outlived its executor would leave the wizard reporting
	// a terminal that is not there. Empty leaves the current claim
	// untouched, so a heartbeat need not repeat it.
	Driver string `json:"driver,omitempty"`
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
	if !validSetupExecutorStep(req.Step) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid step"})
		return
	}
	if req.CompletedBytes < 0 || req.TotalBytes < 0 || req.RateBps < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "progress figures must be non-negative"})
		return
	}
	// The control plane rejects an unknown driver outright, so an invalid
	// one has to be caught here rather than poisoning every subsequent
	// push from this device.
	if !signer.IsValidSetupDriver(req.Driver) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid driver"})
		return
	}
	writeJSON(w, http.StatusOK, s.setupExecutor.NoteExecutor(r.Context(), req))
}
