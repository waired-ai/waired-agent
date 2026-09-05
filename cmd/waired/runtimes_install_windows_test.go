//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Windows installs the bundled engine exactly the way Linux and macOS do
// since #493, so these mirror runtimes_install_linux_test.go. What went
// with the PowerShell installer: the PSModulePath-stripping test (#178 —
// a Windows PowerShell 5.1 child inheriting pwsh 7's PSModulePath could
// not autoload Get-AuthenticodeSignature) and the argv table for
// -GpuMode / -ModelsDir / -MachineProgress. No PowerShell is spawned any
// more, so neither has a subject.

func TestInstallOllamaWindows_Bundled(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })

	var gotBaseDir string
	var gotDeadline time.Time
	var hadDeadline bool
	gotSink := true
	called := false
	installOllamaBundled = func(ctx context.Context, baseDir string, sink func(infruntime.OllamaInstallProgress)) error {
		called = true
		gotBaseDir = baseDir
		gotSink = sink != nil
		gotDeadline, hadDeadline = ctx.Deadline()
		return nil
	}

	if err := installOllama(true, `C:\ProgramData\waired`, nil); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !called {
		t.Fatal("bundled installer seam was not invoked")
	}
	want := filepath.Join(`C:\ProgramData\waired`, "runtimes", "ollama")
	if gotBaseDir != want {
		t.Errorf("baseDir = %q, want %q", gotBaseDir, want)
	}
	// `waired runtimes install` is a hand-run command: there is no browser
	// wizard on the other end of a lease to report bytes to.
	if gotSink {
		t.Error("a progress sink reached the installer on the hand-run path")
	}
	// The installer must run under the resolved backstop, not a hardcoded
	// wall clock (#189).
	if !hadDeadline {
		t.Fatal("installer ran with no deadline: the install budget is not being applied")
	}
	budget := ollamaInstallTimeout(func(string) string { return "" })
	if got := time.Until(gotDeadline); got > budget || got < budget-time.Minute {
		t.Errorf("install budget = %s, want ~%s (the resolved backstop)", got.Round(time.Second), budget)
	}
}

func TestInstallOllamaWindows_BudgetFollowsTheEnvironment(t *testing.T) {
	t.Setenv(ollamaInstallTimeoutEnv, "3h")

	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	var got time.Duration
	var ok bool
	installOllamaBundled = func(ctx context.Context, _ string, _ func(infruntime.OllamaInstallProgress)) error {
		var dl time.Time
		dl, ok = ctx.Deadline()
		got = time.Until(dl)
		return nil
	}

	if err := installOllama(true, t.TempDir(), nil); err != nil {
		t.Fatalf("installOllama(-y): %v", err)
	}
	if !ok {
		t.Fatal("installer ran with no deadline")
	}
	if got > 3*time.Hour || got < 3*time.Hour-time.Minute {
		t.Errorf("install budget = %s, want ~3h from %s", got.Round(time.Second), ollamaInstallTimeoutEnv)
	}
}

func TestInstallOllamaWindows_Error(t *testing.T) {
	orig := installOllamaBundled
	t.Cleanup(func() { installOllamaBundled = orig })
	sentinel := errors.New("download failed")
	installOllamaBundled = func(ctx context.Context, _ string, _ func(infruntime.OllamaInstallProgress)) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("installer ran with no deadline on the error path either")
		}
		return sentinel
	}
	err := installOllama(true, t.TempDir(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}

// The ROCm overlay is ~250 MB and only the SKUs Ollama's Windows ROCm
// build covers can load it. WAIRED_OLLAMA_GPU_MODE (install.ps1's
// -OllamaGpuMode) forces the answer with the five values it has always
// taken; the base archive already carries CUDA, Vulkan and CPU, which is
// why only 'rocm' asks for anything extra.
func TestWantROCmOverlay_ExplicitModes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"rocm", true},
		{"ROCm", true}, // the flag is not case-sensitive to the user
		{"vulkan", false},
		{"cuda-only", false},
		{"cpu-only", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "WAIRED_OLLAMA_GPU_MODE" {
					return tc.mode
				}
				return ""
			}
			if got := wantROCmOverlay(context.Background(), getenv); got != tc.want {
				t.Errorf("wantROCmOverlay(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// A successful install sweeps the pre-#493 %ProgramFiles%\Ollama — but
// ONLY when waired's own marker is beside the binary. Getting this wrong
// deletes an Ollama the operator installed deliberately.
func TestSweepLegacyOllamaInstall_OnlyOurs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		marker     bool
		wantRemove bool
	}{
		{"waired's own install is swept", true, true},
		{"a user's own install is left alone", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pf := t.TempDir()
			dir := filepath.Join(pf, "Ollama")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ollama.exe"), []byte("MZ"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.marker {
				if err := os.WriteFile(filepath.Join(dir, wairedManagedMarkerName), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			getenv := func(k string) string {
				if k == "ProgramFiles" {
					return pf
				}
				return ""
			}
			var out bytes.Buffer
			sweepLegacyOllamaInstall(getenv, &out)

			_, err := os.Stat(dir)
			removed := os.IsNotExist(err)
			if removed != tc.wantRemove {
				t.Errorf("directory removed = %v, want %v (out: %s)", removed, tc.wantRemove, out.String())
			}
		})
	}
}

// No ProgramFiles in the environment means no legacy layout to reason
// about, and certainly no directory to delete.
func TestSweepLegacyOllamaInstall_NoProgramFiles(t *testing.T) {
	var out bytes.Buffer
	sweepLegacyOllamaInstall(func(string) string { return "" }, &out)
	if out.Len() != 0 {
		t.Errorf("output = %q, want silence", out.String())
	}
}

// pathWithout is what keeps the sweep from rewriting a machine PATH it
// only meant to take one entry out of.
func TestPathWithout(t *testing.T) {
	const path = `C:\Windows;C:\Program Files\Ollama;C:\Program Files\Git\cmd`
	for _, tc := range []struct {
		name    string
		dir     string
		want    string
		changed bool
	}{
		{"exact", `C:\Program Files\Ollama`, `C:\Windows;C:\Program Files\Git\cmd`, true},
		{"case-insensitive", `c:\program files\ollama`, `C:\Windows;C:\Program Files\Git\cmd`, true},
		{"forward slashes", `C:/Program Files/Ollama`, `C:\Windows;C:\Program Files\Git\cmd`, true},
		{"trailing separator", `C:\Program Files\Ollama\`, `C:\Windows;C:\Program Files\Git\cmd`, true},
		{"absent leaves it alone", `C:\Program Files\Nothing`, path, false},
		// The near-miss that must NOT match: a longer path that merely
		// starts with the one we are removing.
		{"prefix is not a match", `C:\Program Files\Oll`, path, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := pathWithout(path, tc.dir)
			if got != tc.want || changed != tc.changed {
				t.Errorf("pathWithout(_, %q) = (%q, %v), want (%q, %v)", tc.dir, got, changed, tc.want, tc.changed)
			}
		})
	}
}

// An empty entry (a stray ';') is dropped rather than turned into a "" the
// loader would read as the current directory.
func TestPathWithout_DropsEmptyEntries(t *testing.T) {
	got, changed := pathWithout(`C:\Windows;;C:\Program Files\Ollama`, `C:\Program Files\Ollama`)
	if got != `C:\Windows` || !changed {
		t.Errorf("pathWithout = (%q, %v), want (%q, true)", got, changed, `C:\Windows`)
	}
}
