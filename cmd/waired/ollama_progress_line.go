package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The Windows engine install runs inside PowerShell, so its transfer
// figures have to cross a process boundary as text before the setup
// executor can republish them (waired-agent#197). scripts/install/
// ollama-windows.ps1 emits one line per tick under -MachineProgress:
//
//	WAIRED-PROGRESS stage=download completed=123 total=456 bps=789
//
// This file is the reader. It lives in an untagged file on purpose: the
// format is a contract between two files that only ever run together on
// Windows, and a parser that can only be tested on Windows is a parser
// that is tested nightly at best (the repo's cross-OS parity rule).
const machineProgressPrefix = "WAIRED-PROGRESS "

// parseOllamaProgressLine reads one machine-progress line. ok is false
// for anything else, including a malformed line: the installer's own
// human output shares this stream, and a stray unparsable line must pass
// through to the operator rather than becoming a bogus progress event.
func parseOllamaProgressLine(line string) (infruntime.OllamaInstallProgress, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), machineProgressPrefix)
	if !found {
		return infruntime.OllamaInstallProgress{}, false
	}
	p := infruntime.OllamaInstallProgress{}
	seen := false
	for _, field := range strings.Fields(rest) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "stage":
			p.Stage = val
		case "completed", "total", "bps":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || n < 0 {
				return infruntime.OllamaInstallProgress{}, false
			}
			switch key {
			case "completed":
				p.Completed = n
			case "total":
				p.Total = n
			case "bps":
				p.BytesPerSec = n
			}
		default:
			continue
		}
		seen = true
	}
	if p.Stage == "" || !seen {
		return infruntime.OllamaInstallProgress{}, false
	}
	return p, true
}

// scanOllamaProgress copies src to out line by line, diverting machine
// progress lines to sink instead of printing them.
//
// Both halves matter. The operator keeps the installer's own narration
// (it is the only thing on screen for several minutes), and the machine
// lines are removed from it rather than added to it — they are an
// implementation detail of the handoff, not something to read.
//
// Returns when src reaches EOF. A sink of nil just strips the lines.
func scanOllamaProgress(src io.Reader, out io.Writer, sink func(infruntime.OllamaInstallProgress)) {
	sc := bufio.NewScanner(src)
	// The installer prints paths and signature subjects; the default 64 KB
	// token limit is ample, but a pathological line must not end the scan
	// and take the rest of the install's output with it.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if p, ok := parseOllamaProgressLine(line); ok {
			if sink != nil {
				sink(p)
			}
			continue
		}
		if out != nil {
			_, _ = io.WriteString(out, line+"\n")
		}
	}
}
