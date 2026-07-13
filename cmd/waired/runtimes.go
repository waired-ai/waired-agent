package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func newRuntimesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtimes",
		Short: "Manage inference runtimes (ls / install / uninstall / refresh / status / benchmark).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newRuntimesLsCmd(),
		newRuntimesInstallCmd(),
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
			return promptBenchmarkRecommendation(mgmt, nonInteractive, os.Stdout, os.Stdin, isTerminal(os.Stdout))
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
			// Serve tuning (#621; absent from old agents).
			ContextLength int    `json:"context_length,omitempty"`
			KVCacheType   string `json:"kv_cache_type,omitempty"`
			NumBatch      int    `json:"num_batch,omitempty"`
			TuningWarning string `json:"tuning_warning,omitempty"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Println(string(body))
		return nil
	}
	if len(resp.Runtimes) == 0 {
		fmt.Println("(no runtimes detected)")
		return nil
	}
	fmt.Printf("%-10s %-10s %-10s %-10s %-12s %s\n", "NAME", "STATE", "INSTALLED", "MODE", "CONTEXT", "VERSION")
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
		// borrowed/adopted modes only the former tells the truth.
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
			if r.NumBatch > 0 { // #642: forced generation ubatch
				tuning += fmt.Sprintf(" b%d", r.NumBatch)
			}
		}
		fmt.Printf("%-10s %-10s %-10s %-10s %-12s %s\n", name, stateOrDash(r.State), installed, mode, tuning, version)
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
		fmt.Printf("⚠ %s\n", w)
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
		fmt.Printf("Recommended engine: %s\n", engine)
	}

	switch engine {
	case "vllm":
		vllmDir := filepath.Join(*stateDir, "runtimes", "vllm")
		if !*yes && !confirmTTY(fmt.Sprintf("Install vllm %s into %s ? (~6 GB)", infruntime.VLLMPinnedVersion, vllmDir)) {
			return errors.New("aborted by user")
		}
		return installVLLM(*stateDir)
	case "ollama":
		return installOllama(*yes, *stateDir)
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
			fmt.Printf("Uninstalled vllm %s\n", active.Version)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive confirmation")
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "agent state dir (vllm venv lives under <state-dir>/runtimes/vllm)")
	return cmd
}

// newRuntimesRefreshCmd re-evaluates engine + model picks against the
// live agent. With --to X, it forces the engine to X; otherwise it
// shows what the agent currently has and suggests a refresh path.
func newRuntimesRefreshCmd() *cobra.Command {
	var to string
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
				fmt.Printf("Active: runtime=%v model=%v variant=%v\n",
					active["runtime"], active["model_id"], active["variant_id"])
			} else {
				fmt.Println("Active: (none — run `waired runtimes install --auto`)")
			}
			if avail == nil {
				fmt.Println("No update available; current pick is already optimal for this host.")
				return nil
			}
			fmt.Printf("Update available: runtime=%v model=%v variant=%v precached=%v\n",
				avail["runtime"], avail["model_id"], avail["variant_id"], avail["precached"])
			_ = to
			if !yes && !confirmTTY("Apply this update?") {
				return errors.New("aborted by user")
			}
			// Step 12 will wire the actual swap command via the management API;
			// for Step 11 the CLI surfaces the suggestion and the operator
			// re-runs `waired models pull` + restart to actually swap.
			fmt.Println("To apply, restart waired-agent (Step 12 zero-downtime swap will land via /waired/v1/runtimes/refresh).")
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "force engine choice (\"\" / ollama / vllm)")
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
		fmt.Println(string(body))
		return nil
	}
	subState, _ := st["subsystem_state"].(string)
	fmt.Printf("subsystem_state: %s\n", subState)
	if runtimes, ok := st["runtimes"].(map[string]interface{}); ok {
		for name, raw := range runtimes {
			r, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			mode, _ := r["mode"].(string)
			if mode == "" {
				continue // old agent without provenance fields
			}
			live, _ := r["live_version"].(string)
			pinned, _ := r["pinned_version"].(string)
			line := fmt.Sprintf("engine: %s mode=%s", name, mode)
			if live != "" {
				line += " live=" + live
			}
			if pinned != "" {
				line += " pinned=" + pinned
			}
			fmt.Println(line)
			if w, _ := r["version_warning"].(string); w != "" {
				fmt.Printf("  ⚠ %s\n", w)
			}
			if e, _ := r["last_error"].(string); e != "" {
				fmt.Printf("  error: %s\n", e)
			}
		}
	}
	if active, ok := st["active"].(map[string]interface{}); ok && active != nil {
		fmt.Printf("active: %v / %v / %v\n", active["runtime"], active["model_id"], active["variant_id"])
	}
	if avail, ok := st["available_update"].(map[string]interface{}); ok && avail != nil {
		fmt.Printf("available_update: %v / %v (precached=%v, swap≈%vs)\n",
			avail["runtime"], avail["model_id"], avail["precached"], avail["expected_swap_seconds"])
		fmt.Println("  Run `waired runtimes refresh` to evaluate switching.")
	}
	printLongContextBench(st)
	return nil
}

// printLongContextBench renders the #624 depth-aware benchmark block
// from the inference status payload. Silent when absent (old agents,
// sweep not run yet).
func printLongContextBench(st map[string]interface{}) {
	lc, ok := st["long_context"].(map[string]interface{})
	if !ok || lc == nil {
		return
	}
	stages, _ := lc["stages"].([]interface{})
	if len(stages) == 0 {
		return
	}
	suffix := ""
	if done, _ := lc["completed"].(bool); !done {
		suffix = " (partial)"
	}
	if ctxLen, ok := lc["context_length"].(float64); ok && ctxLen > 0 {
		suffix = fmt.Sprintf(" @ window %dk%s", int(ctxLen)/1024, suffix)
	}
	fmt.Printf("long-context:%s\n", suffix)
	for _, raw := range stages {
		s, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		target, _ := s["target_tokens"].(float64)
		if failed, _ := s["failed"].(bool); failed {
			fmt.Printf("  %3dk: measurement failed\n", int(target)/1024)
			continue
		}
		prefill, _ := s["prefill_tok_s"].(float64)
		decode, _ := s["decode_tok_s"].(float64)
		fmt.Printf("  %3dk: prefill %.0f tok/s, decode %.1f tok/s\n",
			int(target)/1024, prefill, decode)
	}
}

// recommendEngine asks the agent's /waired/v1/inference/hardware
// endpoint and applies the same auto-pick rule as the bootstrap
// (router.PickEngine semantics, replicated here against the trimmed
// /hardware payload). It honours router.VLLMAutoSelectable so the CLI
// never recommends installing vLLM while its serving path is unwired
// (#557); an explicit --prefer still wins via the early return above.
func recommendEngine(prefer string) (string, error) {
	if prefer != "" {
		return prefer, nil
	}
	body, err := httpGet(defaultMgmtURL + "/waired/v1/inference/hardware")
	if err != nil {
		return "", err
	}
	var hw struct {
		GPUs []struct {
			Vendor      string `json:"vendor"`
			VRAMTotalMB int    `json:"vram_total_mb"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal(body, &hw); err != nil {
		return "", err
	}
	for _, g := range hw.GPUs {
		if strings.EqualFold(g.Vendor, "nvidia") && g.VRAMTotalMB >= 8*1024 && router.VLLMAutoSelectable {
			return "vllm", nil
		}
	}
	return "ollama", nil
}

// vllmInstall is a seam so tests exercise installVLLM's path/ownership
// orchestration without building a real ~6 GB venv. It roots the install
// at the given baseDir (see NewVLLMInstallerAt's #525 rationale).
var vllmInstall = func(ctx context.Context, baseDir string, onProgress func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
	return infruntime.NewVLLMInstallerAt(baseDir).Install(ctx, infruntime.InstallOpts{}, onProgress)
}

// installVLLM drives VLLMInstaller and renders the staged progress to
// stdout in the "[N/5] stage..." format the plan described. The venv is
// rooted at <state-dir>/runtimes/vllm — the same path the daemon resolves
// — so a sudo-run install isn't stranded under root's home (#525).
func installVLLM(stateDir string) error {
	baseDir := filepath.Join(stateDir, "runtimes", "vllm")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := vllmInstall(ctx, baseDir, func(p infruntime.InstallProgress) {
		pct := ""
		if p.Percent >= 0 {
			pct = fmt.Sprintf(" %d%%", p.Percent)
		}
		fmt.Printf("[%d/%d %s]%s %s\n", p.Step, p.Total, p.Stage, pct, p.Message)
	})
	if err != nil {
		return err
	}
	// The venv was just built under sudo (root-owned); hand the state dir
	// back to the waired-agent service user so the daemon can read/manage
	// it — matching the ollama bundle install (#484/#525). No-op off Linux
	// / when not root.
	handStateToServiceUser(stateDir)
	fmt.Printf("\nDone. vLLM %s installed at %s\n", res.Version, res.VenvPath)
	fmt.Println("Run `waired runtimes status` to confirm.")
	// Installing the venv does not switch serving to vLLM — that stays an
	// explicit opt-in (#557) so a single-stream user keeps the faster
	// Ollama path by default. Tell the operator the two things needed to
	// actually serve on vLLM.
	fmt.Println()
	fmt.Println("vLLM serving is opt-in. To switch this host to vLLM:")
	fmt.Println("  1. Set  inference.preferred_engine = \"vllm\"  in agent.json")
	fmt.Println("     (or pass --inference-preferred-engine vllm), then restart waired-agent.")
	fmt.Println("  2. Select a model that ships a vLLM (safetensors) variant, e.g. gpt-oss-20b —")
	fmt.Println("     the default bundled model may be Ollama-only.")
	fmt.Println("Requires an NVIDIA CUDA GPU (compute capability >= 8.0).")
	return nil
}

// confirmTTY prompts the user with prompt + " [y/N]: ". Returns true
// when the answer is y/Y, false otherwise (incl. EOF / non-TTY).
// Tests should pass -y or non-interactive input rather than calling
// this directly.
func confirmTTY(prompt string) bool {
	if !isatty(os.Stdin) {
		return false
	}
	fmt.Printf("%s [y/N]: ", prompt)
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
