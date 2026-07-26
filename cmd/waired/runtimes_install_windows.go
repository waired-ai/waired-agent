//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/pwsh"
	installscripts "github.com/waired-ai/waired-agent/scripts/install"
)

// runOllamaWindowsInstaller is a seam so tests can assert installOllama's
// orchestration without spawning PowerShell.
var runOllamaWindowsInstaller = runOllamaWindowsInstallerImpl

// installOllama (Windows) writes the embedded ollama-windows.ps1 to a
// temp file and runs it via PowerShell with -GpuMode auto. The script
// requires Administrator (it writes under %ProgramFiles%); the tray
// invokes the CLI elevated via UAC RunAs, and a bare invocation should
// be run from an elevated prompt.
func installOllama(yes bool, stateDir string) error {
	// The embedded ps1 installs to %ProgramFiles%\Ollama (the
	// LocalSystem-readable, discovery-first location), so state-dir is
	// not used on Windows.
	_ = stateDir
	if !yes && !confirmTTY("Install Ollama for Windows (downloads the official ZIP into %ProgramFiles%\\Ollama)?") {
		return errors.New("aborted by user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	fmt.Println("Running the Ollama Windows installer (this can take a few minutes)...")
	if err := runOllamaWindowsInstaller(ctx); err != nil {
		return fmt.Errorf("ollama install: %w", err)
	}
	fmt.Println("Ollama installed. waired-agent will adopt it on the next engine start.")
	return nil
}

func runOllamaWindowsInstallerImpl(ctx context.Context) error {
	f, err := os.CreateTemp("", "ollama-install-*.ps1")
	if err != nil {
		return fmt.Errorf("create temp script: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(installscripts.OllamaWindowsPS1); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp script: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp script: %w", err)
	}

	// Own the download staging directory here rather than letting the script
	// mint its own under %TEMP%. The script can only clean up from a
	// `finally`, which does not run when the context deadline terminates the
	// process -- and each abandoned directory holds the ~1.4 GB archive
	// forever, since nothing else ever swept them (#191). This defer runs in
	// the surviving parent even when the child is killed.
	stage, err := os.MkdirTemp("", "ollama-stage-")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	cmd := newOllamaInstallerCmd(ctx, tmp, stage)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// newOllamaInstallerCmd builds the PowerShell invocation of the embedded
// installer script. Split out of runOllamaWindowsInstallerImpl so the argv
// and the child environment are assertable without spawning anything.
func newOllamaInstallerCmd(ctx context.Context, scriptPath, stageDir string) *exec.Cmd {
	// GPU mode / models dir come from the same env knobs the one-liner
	// installer exposes (install.ps1 -OllamaGpuMode / -OllamaModelsDir
	// resolve into these before running `waired init`, which lands here).
	gpuMode := os.Getenv("WAIRED_OLLAMA_GPU_MODE")
	if gpuMode == "" {
		gpuMode = "auto"
	}
	args := []string{
		"-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", scriptPath, "-GpuMode", gpuMode,
	}
	if stageDir != "" {
		args = append(args, "-StageDir", stageDir)
	}
	if d := os.Getenv("WAIRED_OLLAMA_MODELS_DIR"); d != "" {
		args = append(args, "-ModelsDir", d)
	}
	cmd := exec.CommandContext(ctx, "powershell", args...)
	// `powershell` is Windows PowerShell 5.1. It must NOT inherit the
	// PSModulePath of the PowerShell 7 session `waired init` runs under, or
	// Get-AuthenticodeSignature can never autoload and the script's
	// signature check fails as `exit status 1` (#178). Leaving Env nil
	// would inherit the parent environment verbatim.
	cmd.Env = pwsh.Env()
	return cmd
}
