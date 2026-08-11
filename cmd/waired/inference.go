package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// runInference dispatches `waired inference <subverb>`. The
// namespace exists today only for share — the existing
// `waired pause`/`resume`/`infer` verbs predate this group and stay
// at the top level for backward compatibility.
const inferenceLong = `Sub-verbs that toggle inference subsystem behaviour:

  waired inference <on|off|status>   Run AI models on this computer, or stop.
      Persisted across daemon restarts. Turning it on installs the engine and
      downloads the chosen model if they are not there yet.
  waired inference share <on|off|status>   Expose (or stop exposing) this
      agent's local engine to mesh peers. Persisted across daemon restarts.
  waired inference engine <stop|start|status>   Hard-stop the local engine to
      free VRAM/RAM, or restart it. Not persisted.`

func newInferenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inference",
		Short: "Turn local AI on or off, and toggle share / engine behaviour.",
		Long:  inferenceLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newInferenceTransitionCmd("on", state.InferenceEnabled),
		newInferenceTransitionCmd("off", state.InferenceDisabled),
		newInferenceStatusCmd(),
		newInferenceShareCmd(),
		newInferenceEngineCmd(),
	)
	return cmd
}

// newInferenceTransitionCmd builds `waired inference on|off` — whether
// this computer runs AI models itself.
//
// It is the CLI half of #465's opt-in (waired-ai/waired#1056): a host
// below the recommended spec starts with this off, and until now nothing
// in the product could turn it back on. Two remediation strings already
// named this command; they were the only reference to it anywhere.
func newInferenceTransitionCmd(verb string, target state.InferenceState) *cobra.Command {
	var mgmt, stateDir string
	short := "Run AI models on this computer."
	if target == state.InferenceDisabled {
		short = "Stop running AI models on this computer."
	}
	cmd := &cobra.Command{
		Use:   verb,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInferenceTransition(mgmt, stateDir, target, verb)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to persist the choice when the daemon is unreachable")
	return cmd
}

func newInferenceStatusCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether this computer runs AI models itself.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInferenceStatus(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
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

// runInferenceTransition implements `waired inference on|off`. Same
// dual-path shape as share: try the daemon, and persist the choice when
// it is not answering so the next start picks it up.
//
// The fallback carries more weight here than it does for share. The
// machine this command is aimed at is one that is not running AI models,
// and "the daemon is not answering" is a plausible part of why — so a
// version that only worked against a live daemon would be unavailable
// exactly when it is wanted.
func runInferenceTransition(mgmt, stateDir string, target state.InferenceState, verb string) error {
	gf := globalFlags{Mgmt: mgmt, StateDir: stateDir}

	endpoint := "/waired/v1/inference/enable"
	if target == state.InferenceDisabled {
		endpoint = "/waired/v1/inference/disable"
	}

	body, err := httpPost(gf.Mgmt+endpoint, nil)
	if err == nil {
		if target == state.InferenceEnabled {
			fmt.Println("Local inference on. If the engine or the model are not on this " +
				"computer yet, Waired starts fetching them now — watch `waired status`.")
		} else {
			fmt.Println("Local inference off. Waired keeps working: requests go to your " +
				"other computers, or to the cloud.")
		}
		return prettyPrint(body)
	}

	if !isConnectionRefused(err) {
		return fmt.Errorf("waired inference %s: daemon returned: %w", verb, err)
	}
	if writeErr := state.WriteDesiredInferenceState(gf.StateDir, target); writeErr != nil {
		return fmt.Errorf("waired inference %s: daemon unreachable AND could not write desired-inference: %w", verb, writeErr)
	}
	fmt.Printf("waired-agent not running — local inference %s saved; it applies on the next start.\n", verb)
	return nil
}

// inferenceStatusResponse mirrors the subset of management.InferenceStatus
// this command renders.
type inferenceStatusResponse struct {
	SubsystemState string `json:"subsystem_state"`
	DesiredState   string `json:"desired_state"`
	// The daemon's own type rather than a local re-declaration: `waired
	// init` reads the same field now (waired#1099), and two hand-written
	// copies of one wire shape is how a field comes to be read on one
	// surface and silently dropped on the other.
	HostSpeed *management.HostSpeedStatus `json:"host_speed"`
}

// hostSpeedTurnLine is what one coding question costs on this computer,
// as the `waired init` summary box says it (waired-ai/waired-agent#496,
// reported there per waired#1099).
//
// It lives beside the status command deliberately: the two surfaces
// report the same measurement and must not disagree about the NUMBER.
// The sentences differ because a box row and a full sentence are
// different shapes, but the precision does not — one decimal, the same
// %.1f runInferenceStatus below has shipped and the user docs quote
// verbatim.
func hostSpeedTurnLine(hs *management.HostSpeedStatus) string {
	figure := hostSpeedFigure(hs)
	if figure == "" {
		return ""
	}
	return fmt.Sprintf("%s per coding question (comfortable: %.0f s)", figure, hs.BudgetSeconds)
}

// hostSpeedFigure is what this host's measurement says one coding
// question costs. Empty when there is nothing to say.
//
// Two shapes, because the daemon has two (waired-agent#579): a full
// measurement at the canonical depth, and a lower bound taken from the
// prefill rate alone on a host too slow to measure inside the install
// window. `66.9 s` for the first, `210.4 s or more` for the second.
//
// Reading only TurnSeconds is what this function exists to stop. A
// screened host leaves it at zero and fills TurnFloorSeconds, so every
// surface that gated on `TurnSeconds > 0` would have gone silent for
// exactly the machines it most needs to explain itself to — the ones
// Waired has just turned local AI off on.
//
// `or more` and not `at least`: `at least 210.4 s` reads as a
// requirement floor ("you need at least…") in a bare sentence, because a
// reader supplies the missing criterion. The criterion is supplied here
// instead, on its own line — see hostSpeedComparisonLines (owner-approved
// copy, 2026-08-09, waired-agent#579).
func hostSpeedFigure(hs *management.HostSpeedStatus) string {
	switch {
	case hs == nil:
		return ""
	case hs.TurnSeconds > 0:
		return fmt.Sprintf("%.1f s", hs.TurnSeconds)
	case hs.TurnFloorSeconds > 0:
		return fmt.Sprintf("%.1f s or more", hs.TurnFloorSeconds)
	}
	return ""
}

// hostSpeedComparisonLines is the two-row block that puts the figure
// beside the thing it is being judged against, indented by the caller's
// prefix.
//
//	one coding question   210.4 s or more
//	comfortable           45 s or less
//
// A shared renderer because three surfaces print it — init step 6
// interactive, init step 6 non-interactive, and `waired inference
// status` — and they must not disagree about the number or about how it
// is qualified. Empty when there is nothing to compare.
//
// The layout is the point, not decoration. An adjective on the figure
// ("at least 210.4 s") reads as a requirement because the reader has to
// invent the standard being applied; a second row naming the standard
// removes the ambiguity and lets the qualifier stay a plain `or more`.
func hostSpeedComparisonLines(hs *management.HostSpeedStatus, indent string) []string {
	figure := hostSpeedFigure(hs)
	if figure == "" || hs.BudgetSeconds <= 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("%s%-22s%s", indent, "one coding question", figure),
		fmt.Sprintf("%s%-22s%.0f s or less", indent, "comfortable", hs.BudgetSeconds),
	}
}

// hostSpeedMissesBudget reports whether this measurement says the host is
// below the recommended spec.
//
// A bound answers the question as well as a measurement does, and in one
// direction only: the daemon publishes a bound solely once it already
// exceeds the budget (waired-agent#579), so `floor > budget` is the same
// verdict `turn > budget` is, reached more cheaply.
func hostSpeedMissesBudget(hs *management.HostSpeedStatus) bool {
	if hs == nil || hs.BudgetSeconds <= 0 {
		return false
	}
	if hs.TurnSeconds > 0 {
		return hs.TurnSeconds > hs.BudgetSeconds
	}
	return hs.TurnFloorSeconds > hs.BudgetSeconds
}

// hostSpeedBelowSpecLine is the judgement itself, in the term the #465
// ruling pinned (docs-site/TRANSLATION.md: 推奨要件未満). The daemon log
// prints the same words, so an operator reading both sees one claim.
const hostSpeedBelowSpecLine = "This computer is below the recommended spec for running AI locally."

// hostSpeedNotRecommendedLine is the plain-language consequence of the
// line above. It goes AFTER the figures, so it reads as a conclusion
// drawn from them rather than as an echo of the heading.
const hostSpeedNotRecommendedLine = "Running AI locally is not recommended here."

// fetchHostSpeed reads the measurement off the daemon for the `waired
// init` summary. Best-effort by construction: a daemon that cannot be
// reached, an older one with no such field, or a host that was never
// measured all yield nil, and the summary simply says nothing about
// speed. Nothing here may fail an install (waired#1099).
func fetchHostSpeed(mgmt string) *management.HostSpeedStatus {
	body, err := httpGet(mgmt + "/waired/v1/inference/status")
	if err != nil {
		return nil
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return nil
	}
	if hostSpeedFigure(s.HostSpeed) == "" {
		return nil
	}
	// The causal claim only holds while the toggle actually reads off.
	// The daemon drops it when anyone moves the toggle themselves, but it
	// cannot drop it for a state it has not been asked about — so the two
	// are checked together, exactly as `waired inference status` does.
	if s.DesiredState != string(state.InferenceDisabled) {
		s.HostSpeed.TurnedInferenceOff = false
	}
	return s.HostSpeed
}

// inferenceNoStateLine phrases the case where the daemon reported no
// desired state at all.
//
// The `default` arm's reasoning was right and its assumption about who
// lands there was wrong. An empty desired_state does mean "the daemon did
// not say" — but on the CURRENT build the machine that produces it is a
// fresh, not-yet-enrolled install, not an old daemon: sbInfProvider.Status
// returns the zero value when there is no live session, and desired_state
// is `omitempty`, so the key vanishes. Every new user passes through that
// state between installing and signing in, and every one of them was told
// their daemon was too old and sent to `waired update` — advice that is
// both wrong and unactionable, since update reports the host is current
// (#628).
//
// enrolled is the daemon's own answer, and nil means it did not give one.
// Only an explicit false is treated as not-enrolled: per daemonIdentity's
// contract a nil is "unknown", never "no". So the old-daemon branch keeps
// exactly the case it was written for — a daemon that answers neither
// question.
func inferenceNoStateLine(enrolled *bool) string {
	if enrolled != nil && !*enrolled {
		// Same story `waired status` and `waired auth status` tell in this
		// state, so the three commands do not disagree about the machine.
		return "Local inference: not set up yet — this device is not signed in. Run `waired init`."
	}
	return "Local inference: unknown (this daemon does not report it — `waired update`)"
}

// daemonEnrolled asks the daemon whether this device is signed in, for the
// one branch that needs to tell a fresh install from an old daemon. nil
// means no answer — no daemon, a daemon too old to serve the route, or a
// malformed reply.
//
// It is only called from that branch: the on/off arms already have their
// answer and must not pay for a second request.
func daemonEnrolled(mgmtURL string) *bool {
	v := daemonIdentity(mgmtURL)
	if v == nil {
		return nil
	}
	return &v.Enrolled
}

func runInferenceStatus(mgmt string) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/inference/status")
	if err != nil {
		return fmt.Errorf("waired inference status: %w", err)
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("waired inference status: parse: %w", err)
	}
	switch s.DesiredState {
	case string(state.InferenceEnabled):
		fmt.Println("Local inference: on")
		if figure := hostSpeedFigure(s.HostSpeed); figure != "" {
			fmt.Printf("  One coding question takes %s on this computer (comfortable: %.0f s or less).\n",
				figure, s.HostSpeed.BudgetSeconds)
		}
	case string(state.InferenceDisabled):
		fmt.Println("Local inference: off")
		// Why, when Waired is the one who decided. Reported only while
		// the measurement is still the reason — the daemon drops that
		// claim as soon as anyone moves the toggle themselves, so a
		// person who ran `waired inference off` is not told a story about
		// their own machine being slow.
		//
		// The state comes first here and the judgement first in init step
		// 6, on purpose: this command was ASKED for the state, and step 6
		// is about to ask a question and has to justify it. An earlier
		// draft that led with the judgement here read as a report of what
		// was about to happen rather than of what already had.
		if s.HostSpeed != nil && s.HostSpeed.TurnedInferenceOff && hostSpeedFigure(s.HostSpeed) != "" {
			fmt.Printf("  %s\n", hostSpeedBelowSpecLine)
			for _, line := range hostSpeedComparisonLines(s.HostSpeed, "  ") {
				fmt.Println(line)
			}
			fmt.Println("  It can still use the AI running on your other computers.")
		}
		fmt.Println("  Turn it on with `waired inference on`.")
	default:
		// Nothing to report is not the same as off, and telling someone
		// their AI is off when the daemon simply did not say would send
		// them looking for a setting to change. Which of the two silent
		// daemons this is — old, or not signed in yet — needs a second
		// question; see inferenceNoStateLine.
		fmt.Println(inferenceNoStateLine(daemonEnrolled(gf.Mgmt)))
	}
	if s.SubsystemState != "" {
		fmt.Printf("Inference engine: %s\n", s.SubsystemState)
	}
	return nil
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
		fmt.Printf("Engine power: %s (not managed by waired; stop/start unavailable)\n", s.EnginePower)
	default:
		fmt.Printf("Engine power: %s\n", s.EnginePower)
	}
	if s.SubsystemState != "" {
		fmt.Printf("Inference engine: %s\n", s.SubsystemState)
	}
	return nil
}
