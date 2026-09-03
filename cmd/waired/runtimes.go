package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func newRuntimesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtimes",
		Short: "Manage inference runtimes (ls / install / upgrade / uninstall / refresh / status / benchmark).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newRuntimesLsCmd(),
		newRuntimesInstallCmd(),
		newRuntimesUpgradeCmd(),
		newRuntimesUninstallCmd(),
		newRuntimesRefreshCmd(),
		newRuntimesStatusCmd(),
		newRuntimesBenchmarkCmd(),
	)
	return cmd
}

// newRuntimesBenchmarkCmd runs the on-device interactive-performance check
// against the daemon and, when the active model benches below the
// interactive floor, prompts to switch to a lighter one (issue #133).
// Shares the prompt logic with `waired init`.
func newRuntimesBenchmarkCmd() *cobra.Command {
	var mgmt, stateDir string
	var nonInteractive bool
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Run the on-device interactive-performance check.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A measurement command must not flip the active model as a side
			// effect of piped stdin taking the interactive Yes default (issue
			// #347's secondary bug): without a TTY, force report-only mode.
			if !isTerminal(os.Stdin) {
				nonInteractive = true
			}
			var owner *stdinReader
			if isTerminal(os.Stdin) {
				owner = newStdinReader(os.Stdin)
			}
			return promptBenchmarkRecommendation(mgmt, nonInteractive, stdout, promptReader(owner), isTerminal(os.Stdout))
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "directory holding identity.json (unused; accepted for parity)")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "print the recommendation but never switch automatically")
	return cmd
}

func newRuntimesLsCmd() *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List inference runtimes.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimesLsBody(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json")
	return cmd
}

func runRuntimesLsBody(mgmt string) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/inference/runtimes")
	if err != nil {
		return err
	}
	var resp struct {
		Runtimes []struct {
			Name      string `json:"name,omitempty"`
			Installed bool   `json:"installed"`
			Version   string `json:"version,omitempty"`
			State     string `json:"state"`
			// Provenance (new fields; absent from old agents).
			Mode           string `json:"mode,omitempty"`
			LiveVersion    string `json:"live_version,omitempty"`
			VersionWarning string `json:"version_warning,omitempty"`
			LastError      string `json:"last_error,omitempty"`
			// Whether waired has STOPPED restarting this engine. Not
			// declared here at all before waired-agent#1140, so this
			// command could not tell a stop somebody asked for from one
			// the engine decided on its own.
			FailureLatched bool `json:"failure_latched,omitempty"`
			// Serve tuning (#621; absent from old agents).
			ContextLength int    `json:"context_length,omitempty"`
			KVCacheType   string `json:"kv_cache_type,omitempty"`
			TuningWarning string `json:"tuning_warning,omitempty"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		// ascii: the body did not parse, so it is served verbatim -- folding it
		// would edit a payload the caller is reading raw.
		fmt.Println(string(body))
		return nil
	}
	if len(resp.Runtimes) == 0 {
		fmt.Fprintln(stdout, "(no runtimes detected)")
		return nil
	}
	fmt.Fprintf(stdout, "%-10s %-10s %-10s %-10s %-12s %s\n", "NAME", "STATE", "INSTALLED", "MODE", "CONTEXT", "VERSION")
	warnings := []string{}
	for _, r := range resp.Runtimes {
		name := r.Name
		if name == "" {
			name = "-"
		}
		installed := "no"
		if r.Installed {
			installed = "yes"
		}
		// The live (serving) version beats the binary probe; in
		// adopted mode only the former tells the truth.
		version := r.Version
		if r.LiveVersion != "" {
			version = r.LiveVersion
		}
		if version == "" {
			version = "-"
		}
		mode := r.Mode
		if mode == "" {
			mode = "-"
		}
		// #621: effective context window + KV cache type ("131072 q8_0").
		tuning := "-"
		if r.ContextLength > 0 {
			tuning = fmt.Sprintf("%d", r.ContextLength)
			if r.KVCacheType != "" {
				tuning += " " + r.KVCacheType
			}
		}
		fmt.Fprintf(stdout, "%-10s %-10s %-10s %-10s %-12s %s\n",
			name, runtimeStateWord(r.State, r.FailureLatched), installed, mode, tuning, version)
		if r.VersionWarning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.VersionWarning))
		}
		if r.TuningWarning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.TuningWarning))
		}
		if r.LastError != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", name, r.LastError))
		}
	}
	for _, w := range warnings {
		fmt.Fprintf(stdout, "%s %s\n", emo("⚠", "!"), w)
	}
	return nil
}

// runRuntimesInstall handles `waired runtimes install [<engine>] [--auto]
// [--prefer X] [-y]`. With --auto (or no engine arg) it consults the
// management API for hardware recommendations, prompts the user, and
// runs VLLMInstaller (or prints the equivalent for ollama). Single-
// engine installs (`waired runtimes install vllm`) skip the prompt
// step but still show the install plan.
func newRuntimesInstallCmd() *cobra.Command {
	var prefer, stateDir string
	var auto, yes bool
	cmd := &cobra.Command{
		Use:   "install [engine]",
		Short: "Install an inference engine (ollama / vllm), auto-picking by hardware.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRuntimesInstallBody(auto, prefer, yes, stateDir, args)
		},
	}
	cmd.Flags().BoolVar(&auto, "auto", false, "auto-pick the engine based on hardware")
	cmd.Flags().StringVar(&prefer, "prefer", "", "force engine choice (\"\" / ollama / vllm) when auto-picking")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive confirmation")
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "agent state dir (bundled ollama installs under <state-dir>/runtimes/ollama)")
	return cmd
}

func runRuntimesInstallBody(autoVal bool, preferVal string, yesVal bool, stateDirVal string, rest []string) error {
	auto := &autoVal
	prefer := &preferVal
	yes := &yesVal
	stateDir := &stateDirVal
	engine := ""
	switch {
	case *auto || len(rest) == 0:
		// Auto mode below.
	case len(rest) == 1:
		engine = rest[0]
	default:
		return errors.New("usage: waired runtimes install [<engine>] [--auto] [--prefer X] [-y]")
	}

	if engine == "" {
		var err error
		engine, err = recommendEngine(*prefer)
		if err != nil {
			return fmt.Errorf("auto-pick: %w", err)
		}
		fmt.Fprintf(stdout, "Recommended engine: %s\n", engine)
	}

	switch engine {
	case "vllm":
		vllmDir := filepath.Join(*stateDir, "runtimes", "vllm")
		if !*yes && !confirmTTY(fmt.Sprintf("Install vllm %s into %s ? (~6 GB)", infruntime.VLLMPinnedVersion, vllmDir)) {
			return errors.New("aborted by user")
		}
		return installVLLM(*stateDir)
	case "ollama":
		// No sink: `waired runtimes install` is a hand-run command, with
		// nothing on the other side of a lease to report to.
		return installOllama(*yes, *stateDir, nil)
	default:
		return fmt.Errorf("unknown engine %q (supported: ollama, vllm)", engine)
	}
}

func newRuntimesUninstallCmd() *cobra.Command {
	var yes bool
	var stateDir string
	cmd := &cobra.Command{
		Use:   "uninstall <engine>",
		Short: "Uninstall an inference engine (vllm).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			engine := args[0]
			if engine != "vllm" {
				return fmt.Errorf("uninstall currently only implemented for vllm (got %q)", engine)
			}
			// Resolve the venv from the same <state-dir>/runtimes/vllm the
			// installer wrote, not a $HOME-relative default (#525).
			inst := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm"))
			active, ok := inst.Active()
			if !ok {
				return errors.New("vllm not currently installed")
			}
			if !yes && !confirmTTY(fmt.Sprintf("Remove vllm %s and its venv (~6 GB)?", active.Version)) {
				return errors.New("aborted by user")
			}
			if err := inst.Uninstall(context.Background(), active.Version); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Uninstalled vllm %s\n", active.Version)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive confirmation")
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "agent state dir (vllm venv lives under <state-dir>/runtimes/vllm)")
	return cmd
}

// runtimesRefreshApplyHint is how an engine change is applied.
//
// Unlike a model switch, this one really does still need the restart:
// waired#812's in-process swap is same-engine only, and a cross-engine
// target returns errSwapNeedsRestart so the daemon falls back to the
// supervised restart (cmd/waired-agent/inference.go, SwapPreferredModel).
// So only the stale half of the old sentence is gone — the "Step 12
// zero-downtime swap will land via /waired/v1/runtimes/refresh" promise,
// naming an endpoint that was never built.
const runtimesRefreshApplyHint = "To apply, restart waired-agent — switching engines is the one change that still needs it."

// newRuntimesRefreshCmd re-evaluates engine + model picks against the
// live agent: it shows what the agent currently has and what it would
// pick now. Reporting only — applying an engine change is the restart
// below.
func newRuntimesRefreshCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Re-evaluate engine + model picks against the live agent.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := httpGet(defaultMgmtURL + "/waired/v1/inference/status")
			if err != nil {
				return err
			}
			var st map[string]interface{}
			if err := json.Unmarshal(body, &st); err != nil {
				return err
			}
			active, _ := st["active"].(map[string]interface{})
			avail, _ := st["available_update"].(map[string]interface{})
			if active != nil {
				fmt.Fprintf(stdout, "Active: runtime=%v model=%v variant=%v\n",
					active["runtime"], active["model_id"], active["variant_id"])
			} else {
				fmt.Fprintln(stdout, "Active: (none — run `waired runtimes install --auto`)")
			}
			if avail == nil {
				fmt.Fprintln(stdout, "No update available; current pick is already optimal for this host.")
				return nil
			}
			fmt.Fprintf(stdout, "Update available: runtime=%v model=%v variant=%v precached=%v\n",
				avail["runtime"], avail["model_id"], avail["variant_id"], avail["precached"])
			if !yes && !confirmTTY("Apply this update?") {
				return errors.New("aborted by user")
			}
			fmt.Fprintln(stdout, runtimesRefreshApplyHint)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive confirmation")
	return cmd
}

func newRuntimesStatusCmd() *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show inference runtime status.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuntimesStatusBody(mgmt)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json")
	return cmd
}

func runRuntimesStatusBody(mgmt string) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/inference/status")
	if err != nil {
		return err
	}
	var st map[string]interface{}
	if err := json.Unmarshal(body, &st); err != nil {
		// ascii: the body did not parse, so it is served verbatim -- folding it
		// would edit a payload the caller is reading raw.
		fmt.Println(string(body))
		return nil
	}
	subState, _ := st["subsystem_state"].(string)
	fmt.Fprintf(stdout, "subsystem_state: %s\n", subState)
	if runtimes, ok := st["runtimes"].(map[string]interface{}); ok {
		for name, raw := range runtimes {
			r, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			// mode is ollama provenance (spawned / adopted) and vLLM has
			// none — waired always spawns it, there is no adopt path. This
			// used to `continue` on an empty mode as "old agent without
			// provenance fields", which silently dropped the vLLM row and
			// with it the last_error below: on a host whose engine could
			// not start, `waired runtimes status` printed nothing at all
			// about the engine that failed (waired-agent#1026).
			mode, _ := r["mode"].(string)
			live, _ := r["live_version"].(string)
			pinned, _ := r["pinned_version"].(string)
			line := "engine: " + name
			if mode != "" {
				line += " mode=" + mode
			}
			if live != "" {
				line += " live=" + live
			}
			if pinned != "" {
				line += " pinned=" + pinned
			}
			fmt.Fprintln(stdout, line)
			if w, _ := r["version_warning"].(string); w != "" {
				fmt.Fprintf(stdout, "  %s %s\n", emo("⚠", "!"), w)
			}
			if e, _ := r["last_error"].(string); e != "" {
				fmt.Fprintf(stdout, "  error: %s\n", e)
			}
		}
	}
	if active, ok := st["active"].(map[string]interface{}); ok && active != nil {
		fmt.Fprintf(stdout, "active: %v / %v / %v\n", active["runtime"], active["model_id"], active["variant_id"])
	}
	if avail, ok := st["available_update"].(map[string]interface{}); ok && avail != nil {
		fmt.Fprintf(stdout, "available_update: %v / %v (precached=%v, swap≈%vs)\n",
			avail["runtime"], avail["model_id"], avail["precached"], avail["expected_swap_seconds"])
		fmt.Fprintln(stdout, "  Run `waired runtimes refresh` to evaluate switching.")
	}
	return nil
}

// recommendGPU is the slice of /waired/v1/inference/hardware the engine
// recommendation needs.
type recommendGPU struct {
	Vendor      string `json:"vendor"`
	VRAMTotalMB int    `json:"vram_total_mb"`
}

// recommendEngine asks the agent's /waired/v1/inference/hardware
// endpoint and applies the same auto-pick rule as the bootstrap against
// the trimmed payload. An explicit --prefer wins via the early return.
func recommendEngine(prefer string) (string, error) {
	if prefer != "" {
		return prefer, nil
	}
	body, err := httpGet(defaultMgmtURL + "/waired/v1/inference/hardware")
	if err != nil {
		return "", err
	}
	var hw struct {
		GPUs []recommendGPU `json:"gpus"`
	}
	if err := json.Unmarshal(body, &hw); err != nil {
		return "", err
	}
	// The daemon we just queried is this host's own loopback daemon, so
	// runtime.GOOS is its OS.
	return recommendEngineFor(runtime.GOOS, hw.GPUs), nil
}

// recommendEngineFor is the pure half: which engine should `waired runtimes
// install --auto` put on a host running goos with these GPUs.
//
// It defers to router.VLLMAutoEligible rather than restating the rule. It
// used to carry its own copy — NVIDIA plus 8 GB plus the #557 gate — and
// that copy, like PickEngine's, had no OS term, so on Windows the CLI
// offered to install a vLLM the host can never serve (waired-agent#319).
func recommendEngineFor(goos string, gpus []recommendGPU) string {
	for _, g := range gpus {
		if router.VLLMAutoEligible(goos, g.Vendor, g.VRAMTotalMB) {
			return "vllm"
		}
	}
	return "ollama"
}

// vllmInstall is a seam so tests exercise installVLLM's path/ownership
// orchestration without building a real ~6 GB venv. It roots the install
// at the given baseDir (see NewVLLMInstallerAt's #525 rationale).
var vllmInstall = func(ctx context.Context, baseDir string, recreate bool, onProgress func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
	return infruntime.NewVLLMInstallerAt(baseDir).Install(ctx, infruntime.InstallOpts{Recreate: recreate}, onProgress)
}

// setupVLLMInstallTimeout bounds a vLLM venv build. vLLM's ~6 GB download
// + uv/pip build + CUDA JIT verify runs longer than ollama's tarball, so
// it gets a wider budget than the interactive path used to give it (was
// 30 min). This install runs BEFORE setupResidencyBudget's model wait
// and is not covered by it. The CP's setup window was widened to 60 min
// to cover an install of this length in one wizard session (waired#835
// Phase 2), and since waired#944 it slides rather than staying fixed, so
// neither this timeout nor the residency budget is pinned to it.
const setupVLLMInstallTimeout = 45 * time.Minute

// vllmInstallCore builds (or rebuilds) the vLLM venv rooted at
// <state-dir>/runtimes/vllm — the same path the daemon resolves, so a
// sudo-run install isn't stranded under root's home (#525) — and renders
// staged progress in the "[N/6] stage..." format to stdout. It is the
// shared core of the interactive CLI (installVLLM) and the setup executor
// (installVLLMForSetup); neither the ownership handoff nor the CLI's opt-in
// epilogue lives here, so each caller adds only what its context needs.
//
// sink is the second consumer of the same events — the setup executor's
// lease, so the browser wizard draws the download the terminal is drawing
// (waired-agent#255). It may be nil, which is what a hand-run install
// looks like; the two are peers and neither may suppress the other.
func vllmInstallCore(ctx context.Context, stateDir string, recreate bool, sink func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
	baseDir := filepath.Join(stateDir, "runtimes", "vllm")
	return vllmInstall(ctx, baseDir, recreate, teeProgress(renderVLLMInstallProgress(stdout), sink))
}

// renderVLLMInstallProgress is the terminal half: one line per event in
// the "[N/5 stage]" format, with the transfer figures appended once uv
// has announced a download.
//
// Until #255 there were no figures to append — uv hides its progress bar
// when stderr is not a terminal, so the percentage this used to print was
// never populated on the pip-install stage and the operator watched a
// silent ~4 GB download. The byte grammar matches the ollama renderer's
// so the two engines read the same on one terminal.
func renderVLLMInstallProgress(w io.Writer) func(infruntime.InstallProgress) {
	return func(p infruntime.InstallProgress) {
		var detail string
		switch {
		case p.TotalBytes > 0:
			detail = fmt.Sprintf(" %3d%%  %s / %s",
				int(float64(p.CompletedBytes)/float64(p.TotalBytes)*100),
				download.HumanBytes(p.CompletedBytes), download.HumanBytes(p.TotalBytes))
			if p.BytesPerSec > 0 {
				detail += fmt.Sprintf(" (%s/s)", download.HumanBytes(p.BytesPerSec))
			}
		case p.Percent >= 0:
			detail = fmt.Sprintf(" %d%%", p.Percent)
		}
		_, _ = fmt.Fprintf(w, "[%d/%d %s]%s %s\n", p.Step, p.Total, p.Stage, detail, p.Message)
	}
}

// installVLLMForSetup is the setup executor's vLLM install: the shared venv
// build with the wider budget, WITHOUT the interactive CLI's opt-in epilogue
// (the browser wizard IS the opt-in) and WITHOUT the ownership handoff (the
// executor's install path calls setupHandState itself, mirroring the ollama
// seam). Returns the installer's error verbatim so the wizard shows the real
// reason.
func installVLLMForSetup(stateDir string, sink func(infruntime.InstallProgress)) error {
	ctx, cancel := context.WithTimeout(context.Background(), setupVLLMInstallTimeout)
	defer cancel()
	_, err := vllmInstallCore(ctx, stateDir, true, sink)
	return err
}

// installVLLM drives VLLMInstaller and renders the staged progress to
// stdout in the "[N/6] stage..." format the plan described.
func installVLLM(stateDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), setupVLLMInstallTimeout)
	defer cancel()
	// No sink: `waired runtimes install` is a hand-run command, with
	// nothing on the other side of a lease to report to.
	res, err := vllmInstallCore(ctx, stateDir, true, nil)
	if err != nil {
		return err
	}
	// The venv was just built under sudo (root-owned); hand the state dir
	// back to the waired-agent service user so the daemon can read/manage
	// it — matching the ollama bundle install (#484/#525). No-op off Linux
	// / when not root.
	handStateToServiceUser(stateDir)
	fmt.Fprintf(stdout, "\nDone. vLLM %s installed at %s\n", res.Version, res.VenvPath)
	fmt.Fprintln(stdout, "Run `waired runtimes status` to confirm.")
	// Last, and under their own heading, because an advisory is worth
	// more of the operator's attention than the opt-in instructions
	// below it (waired-agent#898). Printed rather than returned as an
	// error — the build succeeded and is still theirs to keep.
	//
	// TWO headings, because they are not one kind of thing. A missing
	// compiler means the engine will not start; a bundled CUDA whose own
	// pieces disagree is inert while a host toolkit is present, and says
	// so in its own text. Printing both under "This host cannot start
	// the engine yet" made that sentence false on every correctly
	// provisioned host — and since the current pin set produces the
	// skew, that was most of them (waired-agent#957).
	renderVLLMAdvisories(stdout, res.Advisories)
	// Installing the venv does not switch serving to vLLM — that stays an
	// explicit opt-in (#557) so a single-stream user keeps the faster
	// Ollama path by default. Tell the operator the two things needed to
	// actually serve on vLLM.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "vLLM serving is opt-in. To switch this host to vLLM:")
	fmt.Fprintln(stdout, "  1. Set  inference.preferred_engine = \"vllm\"  in agent.json")
	fmt.Fprintln(stdout, "     (or pass --inference-preferred-engine vllm), then restart waired-agent.")
	fmt.Fprintln(stdout, "  2. Select a model that ships a vLLM (safetensors) variant, e.g. gpt-oss-20b —")
	fmt.Fprintln(stdout, "     the default bundled model may be Ollama-only.")
	fmt.Fprintln(stdout, "Requires an NVIDIA CUDA GPU (compute capability >= 8.0).")
	return nil
}

// renderVLLMAdvisories prints an install's advisories under headings that
// say what they mean.
//
// Its own function, and tested, because the defect it fixes lived entirely
// here: `vllmToolchainAdvisories` was table-tested on the strings it returns,
// nothing tested what was printed ABOVE them, and the join between a list and
// a heading is invisible until a real install renders it on a real host
// (waired-agent#957).
//
// TWO headings, because these are not one kind of thing. A missing compiler
// means the engine will not start. A bundled CUDA whose own pieces disagree is
// inert while a host toolkit is present — its own text says so — and the
// current pin set produces that skew, so announcing it under "This host cannot
// start the engine yet" was a false statement on most correctly provisioned
// hosts.
func renderVLLMAdvisories(w io.Writer, advisories []infruntime.VLLMAdvisory) {
	var blocking, notes []string
	for _, a := range advisories {
		if a.Blocking {
			blocking = append(blocking, a.Text)
			continue
		}
		notes = append(notes, a.Text)
	}

	if len(blocking) > 0 {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "This host cannot start the engine yet:")
		for _, a := range blocking {
			_, _ = fmt.Fprintln(w, "  - "+a)
		}
	}
	if len(notes) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	// Leads with the fact the old wording denied: whoever reads no further
	// than the heading should come away with the right belief, and the
	// heading is the part that gets read. Only claimed when nothing blocks —
	// with a blocker above it, "the engine will start" would contradict it.
	if len(blocking) == 0 {
		_, _ = fmt.Fprintln(w, "The engine will start. Worth knowing:")
	} else {
		_, _ = fmt.Fprintln(w, "Also worth knowing:")
	}
	for _, a := range notes {
		_, _ = fmt.Fprintln(w, "  - "+a)
	}
}

// confirmTTY prompts the user with prompt + " [y/N]: ". Returns true
// when the answer is y/Y, false otherwise (incl. EOF / non-TTY).
// Tests should pass -y or non-interactive input rather than calling
// this directly.
func confirmTTY(prompt string) bool {
	if !isatty(os.Stdin) {
		return false
	}
	fmt.Fprintf(stdout, "%s [y/N]: ", prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func isatty(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// runtimeStateWord is the STATE column, which is a word for a person and
// not the wire value.
//
// A give-up latch reaches the wire as state "stopped" — Stop() overwrites
// the whole Health struct with no give-up guard, so a model switch or a
// reconcile bounce after the give-up leaves it there. "stopped" is what a
// person gets after asking for one, so printing it for an engine that
// stopped itself and will not come back said the opposite of the ⚠ line a
// few rows below (waired-agent#1140).
//
// Fits the %-10s column, which is why it is not a sentence; the reason is
// on the ⚠ line and the remediation is in `waired inference engine status`.
func runtimeStateWord(state string, latched bool) string {
	if latched {
		return "gave up"
	}
	return stateOrDash(state)
}
