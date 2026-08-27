package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// runInference dispatches `waired inference <subverb>`. The
// namespace exists today only for share — the existing
// `waired pause`/`resume`/`infer` verbs predate this group and stay
// at the top level for backward compatibility.
const inferenceLong = `Sub-verbs that toggle inference subsystem behaviour:

  waired inference <on|off|status>   Run models on this computer, or stop.
      Persisted across daemon restarts. Turning it on installs the engine and
      downloads the chosen model if they are not there yet.
  waired inference share <on|off|status>   Expose (or stop exposing) this
      agent's local engine to mesh peers. Persisted across daemon restarts.
  waired inference engine <stop|start|status>   Hard-stop the local engine to
      free VRAM/RAM, or restart it. Not persisted.
  waired inference memory <status|remeasure>   Show the free-memory
      measurement model-fit decisions are based on, or take it again.
  waired inference unload   Free the model's memory without stopping the
      engine. The next request loads it again.
  waired inference residency [duration]   Show or set how long the model
      stays loaded after the last request (keep-alive). 0 or "never" keeps it loaded.`

func newInferenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inference",
		Short: "Turn local inference on or off, and toggle share / engine behaviour.",
		Long:  inferenceLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newInferenceTransitionCmd("on", state.InferenceEnabled),
		newInferenceTransitionCmd("off", state.InferenceDisabled),
		newInferenceStatusCmd(),
		newInferenceShareCmd(),
		newInferenceEngineCmd(),
		newInferenceMemoryCmd(),
		newInferenceUnloadCmd(),
		newInferenceResidencyCmd(),
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
	short := "Run models on this computer."
	if target == state.InferenceDisabled {
		short = "Stop running models on this computer."
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
		Short: "Show whether this computer runs models itself.",
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
	// Whether DesiredState was WRITTEN or is just the live default. Step 6
	// needs the difference to know whose choice it would be overriding
	// (waired#1142); an older daemon omits it, which reads as false — the
	// pre-#1142 behaviour.
	DesiredStateSet bool `json:"desired_state_set"`
	// The daemon's own type rather than a local re-declaration: `waired
	// init` reads the same field now (waired#1099), and two hand-written
	// copies of one wire shape is how a field comes to be read on one
	// surface and silently dropped on the other.
	HostSpeed *management.HostSpeedStatus `json:"host_speed"`
	// How far the measurement has got. Step 6 reads it to end a wait for
	// a re-measurement that stopped without producing one
	// (waired-agent#703); an older daemon omits it, which reads as empty
	// and keeps waiting, the pre-#703 behaviour.
	HostSpeedStage string `json:"host_speed_stage"`
	// The available-memory measurement every fit decision on
	// this host rests on (waired-agent#568). The daemon's own type, for
	// the same reason HostSpeed above uses it. An older daemon omits it,
	// which reads as nil and prints nothing.
	HostMemory *management.HostMemoryMeasurement `json:"host_memory"`
}

// hostMemoryLine reports the memory measurement fit decisions are based
// on, or "" when there is nothing to report.
//
// Worth a line at all because the figure is NOT live and reads exactly
// like something that is: it is taken at daemon start before the engine
// starts, and the highest such reading is what stands (#835). An
// operator looking at a fit verdict has no other way to see what it was
// based on, or how old that is (waired-agent#589).
func hostMemoryLine(m *management.HostMemoryMeasurement) string {
	if m == nil || m.AvailableGB <= 0 {
		return ""
	}
	// Date only. The figure describes a startup, and a wall-clock time
	// would suggest a precision the number does not have.
	when := m.MeasuredAt
	if t, err := time.Parse(time.RFC3339, m.MeasuredAt); err == nil {
		when = t.Format("2006-01-02")
	}
	of := ""
	if m.TotalGB > 0 {
		of = fmt.Sprintf(" of %d GB", m.TotalGB)
	}
	if when == "" {
		return fmt.Sprintf("Free memory measured at startup: %d GB%s.", m.AvailableGB, of)
	}
	return fmt.Sprintf("Free memory measured at startup: %d GB%s (measured %s).",
		m.AvailableGB, of, when)
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
	return fmt.Sprintf("%s per request (target: %.0f s or less)", figure, hs.BudgetSeconds)
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
//	per request           210.4 s or more
//	target                45 s or less
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
		fmt.Sprintf("%s%-22s%s", indent, "per request", figure),
		fmt.Sprintf("%s%-22s%.0f s or less", indent, "target", hs.BudgetSeconds),
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
const hostSpeedBelowSpecLine = "This computer is below the recommended spec for local inference."

// hostSpeedNotRecommendedLine is the plain-language consequence of the
// line above. It goes AFTER the figures, so it reads as a conclusion
// drawn from them rather than as an echo of the heading.
const hostSpeedNotRecommendedLine = "Local inference is not recommended on this computer."

// fetchInitInferenceFacts reads the inference status the closing summary
// is built from, in ONE request. Best-effort by construction: a daemon
// that cannot be reached or an answer that will not parse yields the
// zero value, and every derivation below reads that as "nothing to say".
// Nothing here may fail an install (waired#1099).
//
// One read rather than one per fact, because the two facts the box needs
// come off the same document and must not be sampled a second apart: the
// measurement, and whether local inference is switched off at all.
func fetchInitInferenceFacts(mgmt string) inferenceStatusResponse {
	body, err := httpGet(mgmt + "/waired/v1/inference/status")
	if err != nil {
		return inferenceStatusResponse{}
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return inferenceStatusResponse{}
	}
	return s
}

// hostSpeedFrom is the measurement half of the summary. A host that was
// never measured, or an older daemon with no such field, yields nil and
// the summary simply says nothing about speed.
func hostSpeedFrom(s inferenceStatusResponse) *management.HostSpeedStatus {
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

// localInferenceOffFrom is the other half: whether this computer will run
// models at all, in the daemon's own words — "disabled" for the toggle
// being off, "stopped" for an engine that is parked, "" for a host that
// serves. It is the same field `waired status` prints and `waired doctor`
// keys its "off on this computer" finding on, read here so the closing
// box cannot disagree with either of them (waired-agent#1027).
//
// Read off the machine at summary time rather than tracked through the
// run, for the reason claudeCardRouted gives (waired-agent#796): the box
// reports the state of the computer, not what this particular run
// happened to do. Three roads end here — --inference-enabled=false, an
// interactive "no" to "Run models on this computer?", and the
// non-interactive decline on a host below the recommended spec — and a
// host that was already switched off before init ran is a fourth. One
// read covers all of them; tracking would have to cover each.
//
// The speed cutoff's own "off" is deliberately NOT special-cased here:
// it reaches its own box first (printDaemonTooSlowBox), which knows the
// more specific reason and names a different remedy.
func localInferenceOffFrom(s inferenceStatusResponse) string {
	switch s.SubsystemState {
	case string(state.InferenceDisabled), "stopped":
		return s.SubsystemState
	case "":
		// An older daemon, or one that has not brought the subsystem up
		// yet, reports no state at all. Fall through to the recorded
		// answer rather than reading silence as "it serves".
	default:
		// no_engine, starting, downloading, ready, engine_failed — none of
		// these is a computer that was switched off, and each has an arm
		// of its own above or below this one.
		return ""
	}
	if s.DesiredState == string(state.InferenceDisabled) {
		return string(state.InferenceDisabled)
	}
	return ""
}

// fetchHostSpeed reads the measurement off the daemon for callers that
// need nothing else from the status document.
func fetchHostSpeed(mgmt string) *management.HostSpeedStatus {
	return hostSpeedFrom(fetchInitInferenceFacts(mgmt))
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
			fmt.Printf("  One request takes %s on this computer (target: %.0f s or less).\n",
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
			fmt.Println("  It can still use the models running on your other computers.")
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
	if line := hostMemoryLine(s.HostMemory); line != "" {
		fmt.Println(line)
	}
	return nil
}

// runInferenceMemoryRemeasure implements `waired inference memory
// remeasure` — the supported replacement for deleting
// runtime/host-memory.json by hand (waired-agent#589).
func runInferenceMemoryRemeasure(mgmt string) error {
	body, err := httpPost(mgmt+"/waired/v1/inference/memory/remeasure", nil)
	if err != nil {
		return fmt.Errorf("waired inference memory remeasure: %w", err)
	}
	var res management.HostMemoryRemeasure
	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("waired inference memory remeasure: parse: %w", err)
	}
	if !res.Measured {
		// Not an error exit: the daemon answered, and the answer is about
		// the host rather than about the request.
		fmt.Println("Memory was not re-measured.")
		if res.Reason != "" {
			fmt.Printf("  %s\n", res.Reason)
		}
		if res.AvailableGB > 0 {
			fmt.Printf("  The figure in force is still %d GB available.\n", res.AvailableGB)
		}
		return nil
	}
	fmt.Printf("Memory re-measured: %d GB available.\n", res.AvailableGB)
	fmt.Println("  Model-fit decisions use this figure until a startup measurement finds more.")
	return nil
}

func runInferenceMemoryStatus(mgmt string) error {
	body, err := httpGet(mgmt + "/waired/v1/inference/status")
	if err != nil {
		return fmt.Errorf("waired inference memory status: %w", err)
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("waired inference memory status: parse: %w", err)
	}
	line := hostMemoryLine(s.HostMemory)
	if line == "" {
		fmt.Println("No memory measurement has been recorded on this computer yet.")
		return nil
	}
	fmt.Println(line)
	fmt.Println("  Model-fit decisions on this computer are based on this figure.")
	fmt.Println("  Take it again with `waired inference memory remeasure`.")
	return nil
}

func newInferenceMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Show or retake the free-memory measurement (status / remeasure).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newMemoryStatusCmd(), newMemoryRemeasureCmd())
	return cmd
}

func newMemoryStatusCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the memory measurement model-fit decisions are based on.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInferenceMemoryStatus(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

func newMemoryRemeasureCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "remeasure",
		Short: "Take the memory measurement again on this computer.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInferenceMemoryRemeasure(mgmt)
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

	// engineWriteTimeout, not the default: a hard stop kills the engine
	// process and waits for the memory to come back, which the daemon
	// bounds at engineStopBudget (15s). With the default budget the CLI
	// reported a timeout while the stop was in fact succeeding — the same
	// defect the tray fixed for itself in waired#316.
	body, err := httpPostWithin(gf.Mgmt+endpoint, nil, engineWriteTimeout)
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

// newInferenceUnloadCmd implements `waired inference unload`
// (waired-agent#861): give the model's memory back without ending the
// ability to serve.
//
// Distinct from `waired inference engine stop`, which frees the same
// memory by stopping the engine process — that also stops answering.
// Every comparable local-LLM application separates the two (LM Studio's
// Eject, `ollama stop <model>`); this is the axis waired was missing,
// and it matters because model residency is now held indefinitely by
// default.
func newInferenceUnloadCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "unload",
		Short: "Free the model's memory, leaving the engine running.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInferenceUnload(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

func runInferenceUnload(mgmt string) error {
	body, err := httpPost(mgmt+"/waired/v1/inference/model/unload", nil)
	if err != nil {
		// 409 = this engine has no unload axis at all
		// (waired-agent#943). The daemon's own sentence is the answer;
		// restating it here would be a second place to keep in step. Note
		// what the 409 is FOR: a 200 would have been rendered by the arm
		// below as "No model was loaded.", which on such a host is false.
		if isMgmtStatus(err, http.StatusConflict) {
			var se *mgmtStatusError
			_ = errors.As(err, &se)
			// httpPost carries the body verbatim, so the JSON the handler
			// wrote is still wrapped up in the message.
			fmt.Println(parseMgmtError(se.StatusCode, []byte(se.Message)).Message)
			return nil
		}
		return err
	}
	var resp management.ModelUnloadResponse
	if json.Unmarshal(body, &resp) != nil || !resp.Unloaded {
		// Nothing was resident. Not an error: the caller wanted the
		// memory back and the memory is back.
		fmt.Println("No model was loaded.")
		return nil
	}
	fmt.Printf("Unloaded %s. The engine is still running; the next request reloads the model.\n", resp.Model)
	return nil
}

// newInferenceResidencyCmd implements `waired inference residency
// [<duration>]` (waired-agent#861): how long the engine keeps the model
// in memory after the last request.
//
// Under `waired inference` rather than `waired config` because it is an
// inference setting with a live half — the same dual-path shape as
// `waired inference share`, which `waired config log-level`'s own doc
// points at as the model for exactly this.
//
// No argument reads the setting; an argument sets it. Reading and
// writing one scalar do not need two verbs, and an operator who types
// the bare command gets the answer rather than a usage error.
func newInferenceResidencyCmd() *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   "residency [duration]",
		Short: "Keep-alive: how long the model stays loaded after the last request.",
		Long: "Show or set how long the engine keeps the model loaded after the last request.\n\n" +
			"With no argument, prints the current setting. With a duration (e.g. 30m, 8h),\n" +
			"sets it. Pass \"always\" (or 0) to keep the model loaded indefinitely, which is\n" +
			"the default: reloading it costs the next request a weights load and a full\n" +
			"prompt re-read. Use `waired inference unload` to free the memory on demand.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runInferenceResidencyShow(mgmt, stateDir)
			}
			return runInferenceResidencySet(mgmt, stateDir, args[0])
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to read/persist the setting when the daemon is unreachable")
	return cmd
}

func runInferenceResidencyShow(mgmt, stateDir string) error {
	body, err := httpGet(mgmt + "/waired/v1/inference/residency")
	if err == nil {
		var resp management.ResidencyResponse
		if jErr := json.Unmarshal(body, &resp); jErr != nil {
			return fmt.Errorf("waired inference residency: parse: %w", jErr)
		}
		fmt.Println(residencySentence(resp, ""))
		// On a host whose engine holds the model for the life of the
		// process, "always" is literally true — and there is still no
		// timeout to set here, which is the half a bare reading would hide
		// (waired-agent#943). A nil Supported is an agent that predates the
		// field and is making no claim, so it prints exactly as before.
		if resp.Supported != nil && !*resp.Supported {
			fmt.Println(residencyUnsupportedNote)
		}
		return nil
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired inference residency: %w", err)
	}
	path := agentconfig.JSONPathFor(stateDir)
	cfg := agentconfig.Defaults()
	if mErr := cfg.MergeJSON(path); mErr != nil {
		return fmt.Errorf("waired inference residency: daemon unreachable AND could not read %s: %w", path, mErr)
	}
	idle := cfg.Inference.IdleTimeout.Duration()
	resp := management.ResidencyResponse{IdleTimeout: idle.String(), HoldsIndefinitely: idle <= 0}
	fmt.Println(residencySentence(resp, " (persisted; waired-agent not running)"))
	return nil
}

func runInferenceResidencySet(mgmt, stateDir, arg string) error {
	idle, err := management.ParseResidency(arg)
	if err != nil {
		return fmt.Errorf("waired inference residency: %q is not a duration (try 30m, 8h, or always): %w", arg, err)
	}
	payload, _ := json.Marshal(management.ResidencyRequest{IdleTimeout: idle.String()})
	body, err := httpPost(mgmt+"/waired/v1/inference/residency", payload)
	if err == nil {
		var resp management.ResidencyResponse
		if jErr := json.Unmarshal(body, &resp); jErr == nil && resp.IdleTimeout != "" {
			fmt.Println(residencySentence(resp, residencyEffectSuffix(resp.Effect)))
			printResidencyCaveat(resp.Effect)
			return nil
		}
		// An agent too old to report an effect: say what was asked for
		// and claim nothing about how it landed.
		fmt.Println(residencySentence(management.ResidencyResponse{
			IdleTimeout: idle.String(), HoldsIndefinitely: idle <= 0,
		}, ""))
		return nil
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired inference residency: daemon returned: %w", err)
	}
	path := agentconfig.JSONPathFor(stateDir)
	cfg := agentconfig.Defaults()
	if mErr := cfg.MergeJSON(path); mErr != nil {
		return fmt.Errorf("waired inference residency: daemon unreachable AND could not read %s: %w", path, mErr)
	}
	cfg.Inference.IdleTimeout = agentconfig.NewDuration(idle)
	if sErr := cfg.Save(path); sErr != nil {
		return fmt.Errorf("waired inference residency: daemon unreachable AND could not persist to %s: %w", path, sErr)
	}
	fmt.Printf("waired-agent not running — setting persisted to %s; applies on next start.\n", path)
	fmt.Println(residencySentence(management.ResidencyResponse{
		IdleTimeout: idle.String(), HoldsIndefinitely: idle <= 0,
	}, ""))
	return nil
}

// residencyEffectSuffix names how the value reached the engine
// (waired-agent#908). Previously every successful write said "(applied
// live)", which was true only when a model happened to be resident: the
// engine reads OLLAMA_KEEP_ALIVE at spawn and the serving path cannot
// carry a per-request keep_alive, so on an empty engine the old claim
// was simply false.
//
// An effect this build does not know renders as no claim at all, which
// is the honest answer for a value it cannot interpret.
func residencyEffectSuffix(e management.ResidencyEffect) string {
	switch e {
	case management.ResidencyEffectLive:
		return " (applied live)"
	case management.ResidencyEffectEngineRestarted:
		return " (the engine restarted to pick it up)"
	case management.ResidencyEffectOnEngineStart:
		return " (applies when the engine starts)"
	case management.ResidencyEffectUnsupported:
		// No claim about this engine: the sentence printed alongside says
		// what is actually true here, and a suffix that also spoke would
		// say it twice.
		return ""
	default:
		return ""
	}
}

// printResidencyCaveat says out loud the one case the setting cannot
// reach on its own: an engine waired did not spawn holds an environment
// waired cannot change (waired-agent#320). Printed rather than folded
// into the suffix because it asks the operator to do something, and
// because a surface may not refuse silently (waired#1067).
func printResidencyCaveat(e management.ResidencyEffect) {
	switch e {
	case management.ResidencyEffectNeedsEngineRestart:
		fmt.Println("This engine was started outside waired, so it keeps the old setting until it is restarted.")
	case management.ResidencyEffectUnsupported:
		// waired#1067: a surface may not refuse silently. The setting is
		// stored — a host that later adopts an engine with this axis finds
		// it — but nothing here will honour it, and saying so is the whole
		// point (waired-agent#943).
		fmt.Println(residencyUnsupportedNote)
	}
}

// residencyUnsupportedNote is what a host whose engine holds the model for
// the life of the process says, on the read and on the write alike.
//
// "the inference engine" as the generic noun (waired-ai/waired#1272), per the owner ruling
// pinned in docs-site/TRANSLATION.md (waired-agent#836/#850). The quoted span
// is exactly a command and nothing else, so it can be copied
// (waired-agent#862).
const residencyUnsupportedNote = "The inference engine on this computer holds the model for as long as the engine runs, " +
	"so there is no keep-alive to set here.\n" +
	"To free the memory, stop the engine: `waired inference engine stop`"

// residencySentence renders the setting the way it is meant to be read.
// The zero is spelled out rather than printed as "0s", which reads as
// "unloads instantly" — the opposite of what it means.
func residencySentence(r management.ResidencyResponse, suffix string) string {
	if r.HoldsIndefinitely {
		return "Keep-alive: always (the model stays loaded)" + suffix + "."
	}
	return "Keep-alive: " + r.IdleTimeout + " after the last request" + suffix + "."
}
