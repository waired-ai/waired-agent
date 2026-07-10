package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// runInference dispatches `waired inference <subverb>`. The
// namespace exists today only for share — the existing
// `waired pause`/`resume`/`infer` verbs predate this group and stay
// at the top level for backward compatibility.
const inferenceLong = `Sub-verbs that toggle inference subsystem behaviour:

  waired inference share <on|off|status>   Expose (or stop exposing) this
      agent's local engine to mesh peers. Persisted across daemon restarts.
  waired inference engine <stop|start|status>   Hard-stop the local engine to
      free VRAM/RAM, or restart it. Not persisted.`

func newInferenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inference",
		Short: "Toggle inference subsystem behaviour (share / engine).",
		Long:  inferenceLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newInferenceShareCmd(), newInferenceEngineCmd())
	return cmd
}

func newInferenceShareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Toggle whether this agent's local engine is exposed to mesh peers (on / off / status).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newShareTransitionCmd("on", state.ShareMeshShared),
		newShareTransitionCmd("off", state.ShareMeshNotShared),
		newShareStatusCmd(),
	)
	return cmd
}

func newShareTransitionCmd(verb string, target state.ShareMeshState) *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   verb,
		Short: fmt.Sprintf("Turn mesh sharing %s.", verb),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShareTransition(mgmt, stateDir, target, "share "+verb)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to persist the share choice when the daemon is unreachable")
	return cmd
}

func newShareStatusCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the local engine is shared with mesh peers.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShareStatus(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

func newInferenceEngineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "engine",
		Short: "Hard-stop / restart the local inference engine (stop / start / status).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newEngineTransitionCmd("stop", true),
		newEngineTransitionCmd("start", false),
		newEngineStatusCmd(),
	)
	return cmd
}

func newEngineTransitionCmd(verb string, stop bool) *cobra.Command {
	var mgmt string
	short := "Restart the local inference engine."
	if stop {
		short = "Hard-stop the local inference engine to free VRAM/RAM."
	}
	cmd := &cobra.Command{
		Use:   verb,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEngineTransition(mgmt, stop, "engine "+verb)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

func newEngineStatusCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the local inference engine power state.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEngineStatus(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

// runShareTransition implements `waired inference share on|off`. Try the
// daemon first, fall back to writing desired-share so the next start picks
// it up — same dual-path pattern as pause/resume.
func runShareTransition(mgmt, stateDir string, target state.ShareMeshState, verb string) error {
	gf := globalFlags{Mgmt: mgmt, StateDir: stateDir}

	endpoint := "/waired/v1/inference/share/enable"
	if target == state.ShareMeshNotShared {
		endpoint = "/waired/v1/inference/share/disable"
	}

	body, err := httpPost(gf.Mgmt+endpoint, nil)
	if err == nil {
		fmt.Printf("%s ok.\n", verb)
		return prettyPrint(body)
	}

	if !isConnectionRefused(err) {
		return fmt.Errorf("waired inference %s: daemon returned: %w", verb, err)
	}
	if writeErr := state.WriteDesiredShareMesh(gf.StateDir, target); writeErr != nil {
		return fmt.Errorf("waired inference %s: daemon unreachable AND could not write desired-share: %w", verb, writeErr)
	}
	fmt.Printf("waired-agent not running — %s persisted; will apply on next start.\n", verb)
	return nil
}

// shareStatusResponse mirrors the subset of management.InferenceStatus
// we render. Kept narrow so a future field on InferenceStatus doesn't
// force a CLI bump.
type shareStatusResponse struct {
	SubsystemState string `json:"subsystem_state"`
	DesiredState   string `json:"desired_state"`
	ShareWithMesh  string `json:"share_with_mesh"`
}

func runShareStatus(mgmt string) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/inference/status")
	if err != nil {
		return fmt.Errorf("waired inference share status: %w", err)
	}
	var s shareStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("waired inference share status: parse: %w", err)
	}
	switch s.ShareWithMesh {
	case "":
		fmt.Println("Share with mesh: unsupported (daemon has no share controller)")
	case string(state.ShareMeshShared):
		fmt.Println("Share with mesh: on")
	case string(state.ShareMeshNotShared):
		fmt.Println("Share with mesh: off")
	default:
		fmt.Printf("Share with mesh: %s (unrecognised — check daemon version)\n", s.ShareWithMesh)
	}
	if s.SubsystemState != "" {
		fmt.Printf("Inference engine: %s\n", s.SubsystemState)
	}
	if s.DesiredState != "" {
		fmt.Printf("Inference toggle: %s\n", s.DesiredState)
	}
	return nil
}

// runEngineTransition implements `waired inference engine stop|start` — the
// hard engine power axis (#186). Unlike `share`, this state is NOT persisted,
// so the daemon-unreachable path has nothing to write: stop is already a
// no-op (no engine running) and start happens on daemon launch.
func runEngineTransition(mgmt string, stop bool, verb string) error {
	gf := globalFlags{Mgmt: mgmt}

	endpoint := "/waired/v1/inference/engine/start"
	if stop {
		endpoint = "/waired/v1/inference/engine/stop"
	}

	body, err := httpPost(gf.Mgmt+endpoint, nil)
	if err == nil {
		fmt.Printf("%s ok.\n", verb)
		return prettyPrint(body)
	}

	if !isConnectionRefused(err) {
		return fmt.Errorf("waired inference %s: daemon returned: %w", verb, err)
	}
	// Engine power is live-only (not persisted). With the daemon down
	// there is no process to act on and nothing to queue.
	if stop {
		fmt.Println("waired-agent not running — engine already stopped (nothing to do).")
	} else {
		fmt.Println("waired-agent not running — the engine starts automatically when the daemon launches.")
	}
	return nil
}

// engineStatusResponse mirrors the subset of management.InferenceStatus the
// engine status view renders.
type engineStatusResponse struct {
	SubsystemState string `json:"subsystem_state"`
	EnginePower    string `json:"engine_power"`
	EngineManaged  bool   `json:"engine_managed"`
}

func runEngineStatus(mgmt string) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/inference/status")
	if err != nil {
		return fmt.Errorf("waired inference engine status: %w", err)
	}
	var s engineStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("waired inference engine status: parse: %w", err)
	}
	switch {
	case s.EnginePower == "":
		fmt.Println("Engine power: unsupported (daemon has no engine controller)")
	case !s.EngineManaged:
		fmt.Printf("Engine power: %s (reused — not managed by waired; stop/start unavailable)\n", s.EnginePower)
	default:
		fmt.Printf("Engine power: %s\n", s.EnginePower)
	}
	if s.SubsystemState != "" {
		fmt.Printf("Inference engine: %s\n", s.SubsystemState)
	}
	return nil
}
