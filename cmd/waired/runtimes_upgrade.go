package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// `waired runtimes upgrade <engine>` brings an ALREADY-INSTALLED engine
// up to this build's pin, and does nothing on a host that has no engine
// (#826 for ollama, #843 for vllm).
//
// It exists as its own verb rather than a flag on `install` because it
// answers a different question. `install` answers "put an engine here";
// this answers "make the engine here match what this build serves with",
// and the difference matters precisely on the host that has none:
// `waired init` is the only thing that decides a computer should run
// models (#138), and an update must not decide it by downloading 1.4 GB.
//
// The installer scripts call it after swapping the binaries and before
// restarting the service, so the service comes up on the converged
// engine. The daemon runs the same policy on start for the hosts that
// never pass through those scripts — `apt upgrade` is the ordinary one.
func newRuntimesUpgradeCmd() *cobra.Command {
	var stateDir string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "upgrade <engine>",
		Short: "Bring an installed inference engine up to this build's pinned version.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runRuntimesUpgradeBody(args[0], stateDir, quiet)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "agent state dir (the bundled engine lives under <state-dir>/runtimes/<engine>)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print nothing when there is nothing to do")
	return cmd
}

func runRuntimesUpgradeBody(engine, stateDir string, quiet bool) error {
	switch engine {
	case "ollama":
	case "vllm":
		return runVLLMUpgrade(stateDir, quiet)
	default:
		return fmt.Errorf("unknown engine %q (supported: ollama, vllm)", engine)
	}

	budget := ollamaInstallTimeout(os.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	baseDir := infruntime.BundledOllamaDir(stateDir)
	inst := infruntime.NewOllamaInstaller(baseDir)
	decision, err := infruntime.ConvergeOllama(ctx, infruntime.OllamaConvergeDeps{
		Present:    inst.Active,
		BinaryPath: inst.BinaryPath,
		Probe: func(ctx context.Context, path string) (bool, string) {
			return hardware.EngineVersionAt(ctx, "ollama", path)
		},
		// The real installer, not a bare fetch: it draws the progress bar
		// a person watching an update expects, overlays ROCm where the
		// hardware wants it, and hands the state dir back to the service
		// user afterwards. yes=true because the decision above IS the
		// confirmation — there is nothing left to ask.
		Install: func(context.Context) error { return installOllama(true, stateDir, nil) },
	})
	if err != nil {
		return err
	}
	if decision.Install || !quiet {
		fmt.Printf("Engine: %s\n", decision.Reason)
	}
	return nil
}

// runVLLMUpgrade is the vllm arm of `waired runtimes upgrade` (#843).
//
// It answers the same question as the ollama arm — "make the engine here
// match what this build serves with" — and refuses the same way on a
// host that has no venv, because installing one is `waired init`'s
// decision (#138) and it costs ~6 GB.
//
// No confirmation prompt, unlike `runtimes install vllm`. The installer
// scripts run this non-interactively during an update, and the decision
// is already the confirmation: a venv that does not match the pin set
// cannot serve what this build claims to serve.
func runVLLMUpgrade(stateDir string, quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), setupVLLMInstallTimeout)
	defer cancel()

	baseDir := filepath.Join(stateDir, "runtimes", "vllm")
	inst := infruntime.NewVLLMInstallerAt(baseDir)
	decision, err := infruntime.ConvergeVLLM(ctx, infruntime.VLLMConvergeDeps{
		Active: func() (string, bool) {
			res, ok := inst.Active()
			return res.Version, ok
		},
		Pins: inst.ActivePins,
		FreeBytes: func() int64 {
			free, err := hardware.FreeDiskBytes(baseDir)
			if err != nil {
				return 0
			}
			return free
		},
		// The real driver, not a bare Install: it renders the staged
		// "[N/5]" progress a person watching an update expects, and
		// hands the state dir back to the service user afterwards, so a
		// venv rebuilt under sudo is one the daemon can still read
		// (#525 / #778).
		Install: func(ctx context.Context) error {
			if _, err := vllmInstallCore(ctx, stateDir, nil); err != nil {
				return err
			}
			handStateToServiceUser(stateDir)
			return nil
		},
		Prune: inst.PruneOtherVersions,
	})
	if err != nil {
		return err
	}
	if decision.Install || decision.Blocked || !quiet {
		fmt.Printf("vLLM: %s\n", decision.Reason)
	}
	// Reclaiming the superseded venv is reported separately from
	// converging, because failing to free ~6 GB is not a failed update.
	if len(decision.Pruned) > 0 {
		fmt.Printf("vLLM: removed the superseded venv(s): %s\n", strings.Join(decision.Pruned, ", "))
	}
	if decision.PruneErr != nil {
		fmt.Printf("⚠ vLLM: could not remove a superseded venv (it still works, it just uses disk): %v\n", decision.PruneErr)
	}
	return nil
}
