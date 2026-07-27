package main

import (
	"bytes"
	"strings"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The Windows installer's machine-progress line is a contract between a
// PowerShell script and this parser. Neither compiles the other, so the
// only thing holding the two together is this test plus the sample lines
// it carries — keep them copy-pasted from ollama-windows.ps1's
// Write-MachineProgress format string.
func TestParseOllamaProgressLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want infruntime.OllamaInstallProgress
		ok   bool
	}{
		{
			name: "base download",
			line: "WAIRED-PROGRESS stage=download completed=1048576 total=1503238553 bps=76281364",
			want: infruntime.OllamaInstallProgress{
				Stage: "download", Completed: 1048576, Total: 1503238553, BytesPerSec: 76281364,
			},
			ok: true,
		},
		{
			name: "rocm overlay keeps its own stage",
			line: "WAIRED-PROGRESS stage=download-rocm completed=10 total=20 bps=5",
			want: infruntime.OllamaInstallProgress{
				Stage: "download-rocm", Completed: 10, Total: 20, BytesPerSec: 5,
			},
			ok: true,
		},
		{
			// A server that omits Content-Length: the script flattens -1 to
			// 0, and 0 means "size unknown" all the way to the wizard, which
			// renders an indeterminate bar rather than a wrong ratio.
			name: "unknown total",
			line: "WAIRED-PROGRESS stage=download completed=4096 total=0 bps=0",
			want: infruntime.OllamaInstallProgress{Stage: "download", Completed: 4096},
			ok:   true,
		},
		{
			name: "leading whitespace from a console that pads",
			line: "   WAIRED-PROGRESS stage=download completed=1 total=2 bps=3",
			want: infruntime.OllamaInstallProgress{Stage: "download", Completed: 1, Total: 2, BytesPerSec: 3},
			ok:   true,
		},
		{name: "ordinary installer output", line: "  archive size: 1,433.6 MB"},
		{name: "a line that merely mentions the prefix", line: "see WAIRED-PROGRESS for details"},
		{name: "no stage", line: "WAIRED-PROGRESS completed=1 total=2 bps=3"},
		{name: "non-numeric count", line: "WAIRED-PROGRESS stage=download completed=lots total=2 bps=3"},
		{name: "negative count", line: "WAIRED-PROGRESS stage=download completed=-1 total=2 bps=3"},
		{name: "empty", line: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOllamaProgressLine(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The operator is watching this stream for minutes; the machine lines are
// plumbing and must not appear in it, and nothing else may be lost.
func TestScanOllamaProgress_SplitsTheStream(t *testing.T) {
	src := strings.Join([]string{
		"Downloading https://example.invalid/ollama-windows-amd64.zip",
		"WAIRED-PROGRESS stage=download completed=1 total=10 bps=100",
		"  50%  (  5.0 / 10.0 MB)  1.0 MB/s",
		"WAIRED-PROGRESS stage=download completed=10 total=10 bps=0",
		"Installed at: C:\\Program Files\\Ollama\\ollama.exe",
	}, "\n") + "\n"

	var out bytes.Buffer
	var got []infruntime.OllamaInstallProgress
	scanOllamaProgress(strings.NewReader(src), &out, func(p infruntime.OllamaInstallProgress) {
		got = append(got, p)
	})

	if len(got) != 2 {
		t.Fatalf("sink saw %d events, want 2: %+v", len(got), got)
	}
	if got[1].Completed != 10 || got[1].Total != 10 {
		t.Errorf("final event = %+v, want the 10/10 completion", got[1])
	}
	if strings.Contains(out.String(), machineProgressPrefix) {
		t.Errorf("machine lines reached the operator:\n%s", out.String())
	}
	for _, want := range []string{"Downloading https://", "50%", "Installed at:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("operator output lost %q:\n%s", want, out.String())
		}
	}
}

// A nil sink still strips the lines: `waired runtimes install` never asks
// for them, but a future caller that scans without subscribing must not
// leak the plumbing onto the console.
func TestScanOllamaProgress_NilSinkStillStrips(t *testing.T) {
	var out bytes.Buffer
	scanOllamaProgress(strings.NewReader("WAIRED-PROGRESS stage=download completed=1 total=2 bps=3\nkeep me\n"), &out, nil)
	if got := out.String(); got != "keep me\n" {
		t.Errorf("out = %q, want just the human line", got)
	}
}

func TestOllamaProgressStage(t *testing.T) {
	// Every transfer stage is the download row; everything else is the
	// install proper. Getting this wrong shows a byte bar on a step that
	// has no bytes, or hides the one that does.
	for stage, want := range map[string]string{
		"download":      "engine_download",
		"download-rocm": "engine_download",
		"extract":       "engine_install",
		"install":       "engine_install",
		"verify":        "engine_install",
		"":              "engine_install",
	} {
		if got := ollamaProgressStage(stage); got != want {
			t.Errorf("ollamaProgressStage(%q) = %q, want %q", stage, got, want)
		}
	}
}

func TestWireRateBps_FlattensTheUnknownSentinel(t *testing.T) {
	// The renderer's -1 ("no sample yet") would be rejected outright by the
	// control plane's validator, and `omitempty` cannot carry it anyway.
	if got := wireRateBps(-1); got != 0 {
		t.Errorf("wireRateBps(-1) = %d, want 0", got)
	}
	if got := wireRateBps(42); got != 42 {
		t.Errorf("wireRateBps(42) = %d, want 42", got)
	}
}

func TestTeeOllamaProgress(t *testing.T) {
	t.Run("nil-only collapses to nil", func(t *testing.T) {
		if teeOllamaProgress(nil, nil) != nil {
			t.Error("want nil so the installer takes its no-callback path")
		}
	})
	t.Run("both peers see every event", func(t *testing.T) {
		var a, b int
		fan := teeOllamaProgress(
			func(infruntime.OllamaInstallProgress) { a++ },
			nil,
			func(infruntime.OllamaInstallProgress) { b++ },
		)
		fan(infruntime.OllamaInstallProgress{Stage: "download"})
		fan(infruntime.OllamaInstallProgress{Stage: "install"})
		if a != 2 || b != 2 {
			t.Errorf("a=%d b=%d, want 2 and 2", a, b)
		}
	})
}
