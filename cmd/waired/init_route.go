package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/proxy/legacycleanup"
)

// promptClaudeRouting asks the deferred end-of-init routing question
// (waired#772): route Claude Code inference through Waired now? It runs
// after the engine setup, model download, and benchmark, so a "yes" flips
// the route at the moment the local stack can actually serve. Default Yes —
// the user already opted into the integration up front. On yes it performs
// exactly what the non-interactive early block / `sudo waired claude
// enable` do: legacy MITM cleanup + the managed-settings write. On no it
// leaves the integration artifacts installed, Claude traffic on the real
// Anthropic API, and prints how to enable later. Returns whether routing
// was enabled.
func promptClaudeRouting(out io.Writer, sc *bufio.Scanner, baseURL, stateDir string) bool {
	return promptClaudeRoutingWith(out, sc, baseURL, func() (string, error) {
		legacycleanup.Run(stateDir, stderrLogger())
		return claudemanaged.WriteWithOptions(baseURL, claudeManagedWriteOptions(stateDir))
	})
}

// promptClaudeRoutingWith is promptClaudeRouting with the apply step
// injectable, so prompt-level tests don't write system managed settings.
func promptClaudeRoutingWith(out io.Writer, sc *bufio.Scanner, baseURL string, apply func() (string, error)) bool {
	writePrompt(out)
	writePromptf(out, "%s %s\n", emo("🔌", "*"), bold("Claude Code request routing"))
	writePrompt(out, "Routing points Claude Code's ANTHROPIC_BASE_URL at your local Waired gateway")
	writePrompt(out, "(no credential; subscription/auto-mode preserved; requests local inference")
	writePrompt(out, "can't serve fall back to the Anthropic API).")
	if !ynPrompt(out, sc, "Route Claude Code inference through Waired now?", true) {
		writePrompt(out, "Routing left off — Claude Code keeps talking to the Anthropic API directly.")
		writePromptf(out, "Enable anytime with `%s`; steer per-session with\n", elevatedCmdline(runtime.GOOS, "waired claude enable"))
		writePrompt(out, "`waired claude route` (or the `/waired-route` skill).")
		return false
	}
	path, err := apply()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warn: writing Claude Code managed settings failed (%v); %s\n", err, elevationHintFor(runtime.GOOS, "waired claude enable"))
		return false
	}
	writePromptf(out, "  %s → ANTHROPIC_BASE_URL=%s (no credential; subscription/auto-mode preserved)\n", path, baseURL)
	return true
}
