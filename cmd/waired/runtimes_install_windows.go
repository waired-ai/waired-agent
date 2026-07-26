//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

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
	budget := ollamaInstallTimeout(os.Getenv)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	fmt.Println("Running the Ollama Windows installer (this can take a few minutes)...")
	if err := runOllamaWindowsInstaller(ctx); err != nil {
		// exec.CommandContext kills the child with TerminateProcess(h, 1),
		// so Wait returns that exit code 1 in preference to the context's
		// own error and the deadline vanishes from the report — the user
		// saw a bare `exit status 1` for what was really a timeout (#189).
		// The script's stall bounds (60 s connect, 120 s per-read) are the
		// real guard; this deadline is a backstop, so say which one hit.
		if ctx.Err() != nil {
			return fmt.Errorf(
				"ollama install: timed out after %s (raise it with %s, e.g. %s=3h): %w",
				budget, ollamaInstallTimeoutEnv, ollamaInstallTimeoutEnv, err)
		}
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
	// Tee stderr: the operator still sees it live, and the tail rides along
	// in the returned error. Handing exec the bare os.Stderr left an
	// *exec.ExitError with no text at all, so the caller could only report
	// `exit status 1` for a script that had just explained itself (#189).
	tail := &tailBuffer{limit: 2048}
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	if err := cmd.Run(); err != nil {
		if s := tail.String(); s != "" {
			return fmt.Errorf("%w: %s", err, s)
		}
		return err
	}
	return nil
}

// tailBuffer keeps the last limit bytes written to it. Bounded because the
// installer prints a progress line several times a second: the interesting
// part of a failure is always the end.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.buf)) }

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
