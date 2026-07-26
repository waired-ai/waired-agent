//go:build windows

package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The regression bar for #178: `waired init` runs under PowerShell 7, and a
// Windows PowerShell 5.1 child that inherits pwsh 7's PSModulePath cannot
// autoload Get-AuthenticodeSignature, so the installer's signature check
// dies and the engine install reports a bare `exit status 1`.
func TestNewOllamaInstallerCmd_DropsPSModulePath(t *testing.T) {
	t.Setenv("PSModulePath", `C:\Program Files\PowerShell\7\Modules`)

	cmd := newOllamaInstallerCmd(context.Background(), `C:\Temp\ollama-install.ps1`, `C:\Temp\ollama-stage-1`)

	if cmd.Env == nil {
		t.Fatal("Env is nil: the child would inherit the parent environment verbatim")
	}
	for _, kv := range cmd.Env {
		if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PSMODULEPATH") {
			t.Errorf("child environment still carries %q", kv)
		}
	}
}

func TestNewOllamaInstallerCmd_Args(t *testing.T) {
	script := `C:\Temp\ollama-install.ps1`

	t.Run("defaults to auto GPU mode and no models dir", func(t *testing.T) {
		t.Setenv("WAIRED_OLLAMA_GPU_MODE", "")
		t.Setenv("WAIRED_OLLAMA_MODELS_DIR", "")

		cmd := newOllamaInstallerCmd(context.Background(), script, "")

		want := []string{
			"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
			"-File", script, "-GpuMode", "auto",
		}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("Args = %q, want %q", cmd.Args, want)
		}
	})

	t.Run("forwards the install.ps1 knobs and the staging dir", func(t *testing.T) {
		t.Setenv("WAIRED_OLLAMA_GPU_MODE", "vulkan")
		t.Setenv("WAIRED_OLLAMA_MODELS_DIR", `D:\ollama\models`)

		cmd := newOllamaInstallerCmd(context.Background(), script, `C:\Temp\ollama-stage-1`)

		// -StageDir is what keeps a context kill from leaking ~1.4 GB: the
		// script's own cleanup lives in a `finally` the terminated process
		// never reaches, so the Go side owns the directory instead (#191).
		want := []string{
			"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
			"-File", script, "-GpuMode", "vulkan",
			"-StageDir", `C:\Temp\ollama-stage-1`,
			"-ModelsDir", `D:\ollama\models`,
		}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("Args = %q, want %q", cmd.Args, want)
		}
	})
}
