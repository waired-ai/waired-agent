package main

import (
	"bytes"
	"strings"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestOllamaInstallRenderer_NonTTY feeds a synthetic install sequence
// (URL announce → streamed byte updates → extract → activate) and asserts
// the please-wait hint prints once, the byte updates draw the shared
// download bar (with the rate), and the stage lines keep their
// established "  [stage] message" shape.
func TestOllamaInstallRenderer_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	render := newOllamaInstallRenderer(&buf, false, "Ollama 0.30.7")
	for _, p := range []infruntime.OllamaInstallProgress{
		{Stage: "download", Message: "https://example.com/ollama-linux-amd64.tar.zst"},
		{Stage: "download", Completed: 100_000_000, Total: 700_000_000, BytesPerSec: -1},
		{Stage: "download", Completed: 350_000_000, Total: 700_000_000, BytesPerSec: 40_000_000},
		{Stage: "download", Completed: 700_000_000, Total: 700_000_000, BytesPerSec: 41_000_000},
		{Stage: "extract", Message: "/var/lib/waired/runtimes/ollama"},
		{Stage: "activate", Message: "/var/lib/waired/runtimes/ollama/bin/ollama"},
	} {
		render(p)
	}
	out := buf.String()

	if !strings.Contains(out, "[download] https://example.com/ollama-linux-amd64.tar.zst") {
		t.Errorf("missing URL announce line, got:\n%s", out)
	}
	if n := strings.Count(out, "Please wait"); n != 1 {
		t.Errorf("please-wait hint printed %d times, want 1; got:\n%s", n, out)
	}
	if !strings.Contains(out, "Downloading Ollama 0.30.7:  50%  350.0 MB / 700.0 MB (40.0 MB/s)") {
		t.Errorf("missing download bar line, got:\n%s", out)
	}
	if !strings.Contains(out, "[extract] /var/lib/waired/runtimes/ollama") ||
		!strings.Contains(out, "[activate] /var/lib/waired/runtimes/ollama/bin/ollama") {
		t.Errorf("stage lines lost their shape, got:\n%s", out)
	}
}

// A ROCm notice (Message-only download-rocm event) must render as a stage
// line, not be mistaken for a byte update.
func TestOllamaInstallRenderer_RocmNotice(t *testing.T) {
	var buf bytes.Buffer
	render := newOllamaInstallRenderer(&buf, false, "Ollama 0.30.7")
	render(infruntime.OllamaInstallProgress{Stage: "download-rocm", Message: "ROCm overlay unavailable; continuing without it"})
	if !strings.Contains(buf.String(), "  [download-rocm] ROCm overlay unavailable") {
		t.Errorf("notice not rendered as a stage line, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Please wait") {
		t.Errorf("notice must not trigger the download hint, got:\n%s", buf.String())
	}
}

// The darwin Ollama.app sequence (URL announce → byte updates → unzip →
// install) must render with the same shapes: hint once, live bar with the
// app label, stage lines for the post-download steps (#615).
func TestOllamaInstallRenderer_DarwinAppSequence(t *testing.T) {
	var buf bytes.Buffer
	render := newOllamaInstallRenderer(&buf, false, "Ollama.app")
	for _, p := range []infruntime.OllamaInstallProgress{
		{Stage: "download", Message: "https://example.com/Ollama-darwin.zip"},
		{Stage: "download", Completed: 200_000_000, Total: 400_000_000, BytesPerSec: 50_000_000},
		{Stage: "download", Completed: 400_000_000, Total: 400_000_000, BytesPerSec: 50_000_000},
		{Stage: "unzip", Message: "/tmp/waired-ollama-x"},
		{Stage: "install", Message: "/Applications"},
	} {
		render(p)
	}
	out := buf.String()
	if n := strings.Count(out, "Please wait"); n != 1 {
		t.Errorf("please-wait hint printed %d times, want 1; got:\n%s", n, out)
	}
	if !strings.Contains(out, "Downloading Ollama.app:  50%  200.0 MB / 400.0 MB (50.0 MB/s)") {
		t.Errorf("missing download bar line, got:\n%s", out)
	}
	if !strings.Contains(out, "[unzip] /tmp/waired-ollama-x") ||
		!strings.Contains(out, "[install] /Applications") {
		t.Errorf("stage lines lost their shape, got:\n%s", out)
	}
}
