package main

// Rendering for `waired runtimes install ollama` progress, shared by the
// Linux bundled-tarball flow (runtimes_install_linux.go) and the macOS
// Ollama.app flow (runtimes_install_darwin.go, #615) — hence no build
// tag.

import (
	"fmt"
	"io"

	"github.com/waired-ai/waired-agent/internal/download"
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
			writePrompt(out, dim(ollamaDownloadHint(p.Stage, p.Total)))
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
//
// The size comes from the transfer rather than the sentence. It used to read
// "a few hundred MB", which was written for macOS's ~129 MB payload and was
// wrong by an order of magnitude on Linux, where the CUDA payload makes it
// 1.4 GB (#661). Any fixed phrase is a claim about a payload that changes
// without this file, so the hint states what the server actually said —
// total is the same figure the progress bar below it counts up to.
//
// total <= 0 means the transfer did not advertise a length (no
// Content-Length); the sentence then makes no size claim at all rather than
// guessing one.
func ollamaDownloadHint(stage string, total int64) string {
	what := "the Ollama engine"
	if stage == "download-rocm" {
		what = "the ROCm GPU runtime"
	}
	if total > 0 {
		return fmt.Sprintf("Downloading %s (%s). This can take a few minutes...",
			what, download.HumanBytes(total))
	}
	return fmt.Sprintf("Downloading %s. This can take a few minutes...", what)
}
