//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

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

	cmd := exec.CommandContext(ctx, "powershell",
		"-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", tmp, "-GpuMode", "auto")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
