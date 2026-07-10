package main

// Rendering for `waired runtimes install ollama` progress, shared by the
// Linux bundled-tarball flow (runtimes_install_linux.go) and the macOS
// Ollama.app flow (runtimes_install_darwin.go, #615) — hence no build
// tag.

import (
	"io"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// newOllamaInstallRenderer renders OllamaInstaller progress. Stage
// transitions and notices keep the established "  [stage] message" lines;
// the byte-level download updates draw the same live bar as the model pull
// (drawDownloadLine — the archive is a multi-hundred-MB transfer that used
// to pass in total silence), preceded by a one-time please-wait hint per
// download stage. label names the download in the bar (Linux:
// "Ollama <version>"; macOS: "Ollama.app").
func newOllamaInstallRenderer(out io.Writer, tty bool, label string) func(infruntime.OllamaInstallProgress) {
	line := downloadLineState{lastPct: -1}
	hinted := map[string]bool{}
	barActive := false
	return func(p infruntime.OllamaInstallProgress) {
		byteUpdate := (p.Stage == "download" || p.Stage == "download-rocm") &&
			(p.Completed > 0 || p.Total != 0)
		if !byteUpdate {
			if barActive { // close the in-place bar before a fresh line
				barActive = false
				if tty {
					writePrompt(out)
				}
				line = downloadLineState{lastPct: -1}
			}
			writePromptf(out, "  [%s] %s\n", p.Stage, p.Message)
			return
		}
		if !hinted[p.Stage] {
			hinted[p.Stage] = true
			writePrompt(out, dim(ollamaDownloadHint(p.Stage)))
		}
		barActive = true
		pct := -1
		if p.Total > 0 {
			pct = int(p.Completed * 100 / p.Total)
		}
		drawDownloadLine(out, tty, &line, label, pct, p.Completed, p.Total, p.BytesPerSec)
	}
}

// ollamaDownloadHint is the one-time please-wait note printed before each
// download bar, mirroring the model pull's multi-GB hint.
func ollamaDownloadHint(stage string) string {
	if stage == "download-rocm" {
		return "Downloading the ROCm GPU runtime — this can take a few minutes. Please wait…"
	}
	return "Downloading the Ollama engine — a few hundred MB; this can take a few minutes. Please wait…"
}
