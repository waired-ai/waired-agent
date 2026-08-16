package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// `waired runtimes upgrade ollama` brings an ALREADY-INSTALLED bundled
// engine up to this build's pin, and does nothing on a host that has no
// engine (#826).
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
		// Out of scope for #826: the vLLM venv is pinned differently and
		// converging it is a ~6 GB rebuild, which wants its own decision
		// about when it is allowed to happen.
		return fmt.Errorf("runtimes upgrade: vllm is not supported yet; use %q", "waired runtimes install vllm")
	default:
		return fmt.Errorf("unknown engine %q (supported: ollama)", engine)
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
