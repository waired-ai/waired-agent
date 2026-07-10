package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Claude Code TUI visibility for waired routing (#580).
//
// Claude Code surfaces routing state to the user through exactly two built-in
// channels: a statusLine command's stdout, and a hook's `systemMessage`. This
// file implements both consumers:
//
//   - `waired claude statusline` renders the always-on footer segment (is waired
//     active / where do requests go / is the agent down). Claude Code runs it on
//     every assistant turn; it self-queries the Management API and must stay fast
//     and never emit an error to stdout (that would corrupt the footer).
//   - `waired claude _fallback-hook` is the Stop-hook worker (installed in
//     managed-settings.json) that emits a one-line `systemMessage` when the turn
//     that just finished fell back to the real Anthropic API.
//
// Plus the install/remove plumbing that edits the user's ~/.claude/settings.json
// statusLine (via the sudo-user hop), and the enable-time detect/prompt flow.

const inferenceStatusPath = "/waired/v1/inference/status"

// statuslineBudget bounds each Management API call the statusline/hook make, so
// a slow or hung agent never stalls Claude Code's footer or turn-end.
const statuslineBudget = 400 * time.Millisecond

// newClaudeStatuslineCmd implements `waired claude statusline` (render) plus its
// `install [--wrap]` / `remove` subcommands. The bare form is what Claude Code
// invokes each turn; the subcommands manage the ~/.claude/settings.json entry
// and are also the targets of the enable/disable sudo-user hop.
func newClaudeStatuslineCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "statusline",
		Short: "Render the Claude Code footer segment showing waired routing (also: install/remove).",
		Long: `Render the Claude Code statusline segment that shows whether waired is
active and where this session's requests currently go. Claude Code runs this
each turn; run it yourself to preview the segment. Subcommands manage the
~/.claude/settings.json entry:

  waired claude statusline                 print the segment (what Claude Code calls)
  waired claude statusline install         add the segment to ~/.claude/settings.json
  waired claude statusline install --wrap  wrap an existing statusLine instead of skipping it
  waired claude statusline remove          remove waired's segment (restores a wrapped one)`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runClaudeStatusline(mgmt) },
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.AddCommand(newClaudeStatuslineInstallCmd(), newClaudeStatuslineRemoveCmd())
	return cmd
}

func newClaudeStatuslineInstallCmd() *cobra.Command {
	var wrap bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install waired's routing statusline into ~/.claude/settings.json.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude statusline install: resolve home: %w", err)
			}
			res, err := claudecode.InstallStatusLine(home, wrap)
			if err != nil {
				return fmt.Errorf("waired claude statusline install: %w", err)
			}
			printStatuslineResult(res)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wrap, "wrap", false, "wrap a pre-existing statusLine (marked, restorable) instead of leaving it")
	return cmd
}

func newClaudeStatuslineRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove waired's statusline from ~/.claude/settings.json (restores a wrapped one).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude statusline remove: resolve home: %w", err)
			}
			if err := claudecode.RemoveStatusLine(home); err != nil {
				return fmt.Errorf("waired claude statusline remove: %w", err)
			}
			fmt.Printf("Removed waired routing statusline from %s\n", claudecode.SettingsPath(home))
			return nil
		},
	}
}

// runClaudeStatusline prints the footer segment. It prints NOTHING (a blank
// segment) unless waired currently owns the Claude route, and never returns an
// error to stdout — any failure degrades to blank or an "agent down" note.
func runClaudeStatusline(mgmt string) error {
	_, present, baseURL := claudemanaged.View()
	if !present || !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		return nil // waired isn't routing Claude Code → blank segment
	}
	route, health, ok := fetchRouteAndHealth(mgmt)
	if !ok {
		fmt.Print(statuslineDown())
		return nil
	}
	fmt.Print(renderStatusline(route, health))
	return nil
}

// fetchRouteAndHealth queries the route state (required) and inference health
// (best-effort) concurrently within the statusline budget. ok=false means the
// agent is unreachable.
func fetchRouteAndHealth(mgmt string) (route management.ClaudeRoutingState, health string, ok bool) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if b, err := fastGet(claudeRouteURL(mgmt), statuslineBudget); err == nil {
			if json.Unmarshal(b, &route) == nil {
				ok = true
			}
		}
	}()
	go func() {
		defer wg.Done()
		if b, err := fastGet(mgmtURL(mgmt, inferenceStatusPath), statuslineBudget); err == nil {
			var h struct {
				SubsystemState string `json:"subsystem_state"`
			}
			if json.Unmarshal(b, &h) == nil {
				health = h.SubsystemState
			}
		}
	}()
	wg.Wait()
	return route, health, ok
}

// renderStatusline builds the colored one-liner. Color is forced (Claude Code
// renders ANSI even though our stdout is a pipe) and gated only on NO_COLOR;
// the glyph degrades to ASCII under WAIRED_NO_EMOJI / a non-UTF-8 locale.
func renderStatusline(route management.ClaudeRoutingState, health string) string {
	mode := route.Policy.Main
	if mode == "" {
		mode = state.ClaudeRouteAuto
	}
	arrow := slGlyph("→", "->")
	// model is appended only on the branches that are actively serving on
	// Waired — a degraded / fell-back / Anthropic segment showing a local
	// model id would misread as "that model answered" (#602).
	model := ""
	if route.LastLocalModel != "" {
		model = " (" + route.LastLocalModel + ")"
	}
	var glyph, label, color string
	switch mode {
	case state.ClaudeRouteAnthropic:
		glyph, label, color = arrow, "waired: Anthropic", ansiYellow
	case state.ClaudeRouteWaired:
		if health == "" || health == "ready" {
			glyph, label, color = slGlyph("⚡", ""), "waired: Waired-only"+model, ansiGreen
		} else {
			glyph, label, color = slGlyph("⚠", "!"), "waired: Waired-only (down)", ansiRed
		}
	default: // auto
		degraded := health != "" && health != "ready"
		recent := route.LastFallback != nil && route.LastFallback.Direction == "anthropic" &&
			time.Since(route.LastFallback.When) < time.Minute
		switch {
		case degraded:
			glyph, label, color = slGlyph("⚡", ""), "waired: fallback "+arrow+" Anthropic (local "+health+")", ansiYellow
		case recent:
			glyph, label, color = slGlyph("⚡", ""), "waired: fell back "+arrow+" Anthropic", ansiYellow
		default:
			glyph, label, color = slGlyph("⚡", ""), "waired: on Waired"+model, ansiGreen
		}
	}
	seg := label
	if glyph != "" {
		seg = glyph + " " + label
	}
	return slSgr(color, seg)
}

func statuslineDown() string {
	return slSgr(ansiRed, strings.TrimSpace(slGlyph("✕", "x")+" waired: agent down"))
}

// --- forced color/glyph for the statusline pipe ------------------------------
//
// The shared style.go helpers gate on stdout being a TTY; here stdout is a pipe
// to Claude Code, which renders ANSI, so we force color unless NO_COLOR is set.

func slColorOn() bool {
	_, disabled := os.LookupEnv("NO_COLOR")
	return !disabled
}

func slSgr(code, s string) string {
	if s == "" || !slColorOn() {
		return s
	}
	return code + s + ansiReset
}

func slGlyph(emoji, ascii string) string {
	if os.Getenv("WAIRED_NO_EMOJI") != "" || !localeIsUTF8() {
		return ascii
	}
	return emoji
}

// --- Stop-hook worker --------------------------------------------------------

// newClaudeFallbackHookCmd is the hidden `waired claude _fallback-hook` worker
// (#580). Claude Code invokes it (as the user) on every Stop event via the
// managed-settings hook. It reads the event JSON on stdin and, when the turn
// that just finished fell back to the real Anthropic API, emits a user-visible
// `systemMessage`. It NEVER blocks stop and always exits 0.
func newClaudeFallbackHookCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:    "_fallback-hook",
		Short:  "internal: Claude Code Stop hook that reports a post-dispatch fallback",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(_ *cobra.Command, _ []string) error { return runFallbackHook(mgmt, os.Stdin, os.Stdout) },
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	return cmd
}

func runFallbackHook(mgmt string, stdin io.Reader, out io.Writer) error {
	// Tolerate an empty / malformed event: session id just defaults to a shared
	// key, and we still de-dup by fallback count.
	var ev struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(stdin).Decode(&ev)

	b, err := fastGet(claudeRouteURL(mgmt), statuslineBudget)
	if err != nil {
		return nil // agent unreachable — say nothing
	}
	var st management.ClaudeRoutingState
	if json.Unmarshal(b, &st) != nil || st.LastFallback == nil {
		return nil
	}
	fb := st.LastFallback
	// Only the "served by Anthropic" direction warrants this notice (the reply
	// did not come from Waired). A local-degrade (anthropic route → local) is a
	// different situation and is surfaced elsewhere.
	if fb.Direction != "anthropic" {
		return nil
	}
	// A fallback counts as "this turn's" only if it is both newer than what this
	// session last saw AND recent (the count is global across sessions, so the
	// recency window guards against attributing another session's fallback here).
	prev, _ := readFallbackCount(ev.SessionID)
	_ = writeFallbackCount(ev.SessionID, fb.Count) // remember where we are regardless
	if fb.Count <= prev || time.Since(fb.When) > 2*time.Minute {
		return nil
	}
	msg := fmt.Sprintf("⚠ waired: this reply came from the real Anthropic API — local inference errored (%s) and waired fell back to keep the turn working. Use /waired-route to switch, or `waired claude route waired` to keep requests strictly on Waired.", fb.Reason)
	payload, err := json.Marshal(map[string]string{"systemMessage": msg})
	if err != nil {
		return nil
	}
	_, _ = fmt.Fprintln(out, string(payload))
	return nil
}

// --- per-session fallback cache ----------------------------------------------

func fallbackCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "waired", "claude-fallback"), nil
}

// sanitizeSession keeps a session id safe as a filename (it is normally a UUID).
func sanitizeSession(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "_nosession"
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

func readFallbackCount(session string) (int64, error) {
	dir, err := fallbackCacheDir()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(filepath.Join(dir, sanitizeSession(session)))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func writeFallbackCount(session string, count int64) error {
	dir, err := fallbackCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pruneFallbackCache(dir)
	return os.WriteFile(filepath.Join(dir, sanitizeSession(session)), []byte(strconv.FormatInt(count, 10)), 0o644)
}

// pruneFallbackCache opportunistically drops per-session entries older than a
// week so the cache dir doesn't grow unbounded across many sessions.
func pruneFallbackCache(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// --- enable/disable wiring (invoking-user hop) -------------------------------

// installStatuslineForInvoker is called by `waired claude enable` and by
// `waired init`. It classifies the invoking user's statusLine and installs
// waired's segment — prompting first when a foreign statusLine would have to
// be wrapped. allowPrompt=false (init --non-interactive) never asks even on a
// TTY: a foreign statusLine gets guidance instead of a blocking y/N read.
// Best-effort: warnings, not failures (the managed-settings write is the core
// of the caller).
func installStatuslineForInvoker(skip, allowPrompt bool) {
	if skip {
		return
	}
	home, viaSudo, sudoUser := invokerHome()
	if home == "" {
		fmt.Fprintln(os.Stderr, "warning: cannot resolve invoking user's home for statusline install")
		return
	}
	kind, existing, err := claudecode.DetectStatusLine(home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", claudecode.SettingsPath(home), err)
		return
	}
	switch kind {
	case claudecode.StatusLineNone, claudecode.StatusLineOurs, claudecode.StatusLineWrapped:
		runStatuslineInstall(viaSudo, sudoUser, home, false)
		warnStatuslineShadow(home)
	case claudecode.StatusLineForeign:
		if !allowPrompt || !stdinIsInteractive() {
			printStatuslineGuidance(existing)
			return
		}
		q := fmt.Sprintf("  You already have a Claude Code statusLine (%s).\n"+
			"  May waired edit ~/.claude/settings.json to also show routing (waired-marked, restored on `waired claude disable`)?", existing)
		if promptYesNo(q) {
			runStatuslineInstall(viaSudo, sudoUser, home, true)
			warnStatuslineShadow(home)
		} else {
			printStatuslineGuidance(existing)
		}
	}
}

// removeStatuslineForInvoker mirrors installStatuslineForInvoker for `waired
// claude disable`.
func removeStatuslineForInvoker() {
	home, viaSudo, sudoUser := invokerHome()
	if home == "" {
		return
	}
	if viaSudo {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runLinkAllAsUser(ctx, sudoUser, []string{"claude", "statusline", "remove"}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "warning: removing waired statusline for user %q failed: %v\n", sudoUser, err)
		}
		return
	}
	if err := claudecode.RemoveStatusLine(home); err != nil {
		fmt.Fprintf(os.Stderr, "warning: removing waired statusline failed: %v\n", err)
	}
}

func runStatuslineInstall(viaSudo bool, sudoUser, home string, wrap bool) {
	if viaSudo {
		args := []string{"claude", "statusline", "install"}
		if wrap {
			args = append(args, "--wrap")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runLinkAllAsUser(ctx, sudoUser, args, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "warning: installing waired statusline for user %q failed: %v\n", sudoUser, err)
		}
		return
	}
	res, err := claudecode.InstallStatusLine(home, wrap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: installing waired statusline failed: %v\n", err)
		return
	}
	printStatuslineResult(res)
}

func printStatuslineResult(res claudecode.StatusLineResult) {
	switch res.Action {
	case "injected":
		fmt.Printf("  Installed waired routing statusline in %s (restart the Claude session to see it).\n", res.Path)
		fmt.Println("  Note: a project-level statusLine (.claude/settings.local.json / settings.json) takes precedence over it — `waired claude status` run inside a project reports shadowing.")
	case "refreshed":
		fmt.Printf("  waired routing statusline present in %s.\n", res.Path)
	case "wrapped":
		fmt.Printf("  Wrapped your existing statusLine in %s (restored on `waired claude disable`).\n", res.Path)
	case "already-wrapped":
		fmt.Println("  waired routing statusline already active.")
	case "skipped-foreign":
		printStatuslineGuidance(res.Existing)
	}
}

func printStatuslineGuidance(existing string) {
	fmt.Printf("  You already have a Claude Code statusLine (%s); left unchanged.\n", existing)
	fmt.Println("  To also show waired routing, run: waired claude statusline install --wrap")
}

// statuslineSnippet is the one-liner users append to their own statusline
// script to show waired's routing segment alongside it (for statusLines in
// scopes waired never edits — project files, managed settings).
const statuslineSnippet = `seg="$(waired claude statusline 2>/dev/null)" && printf ' %s' "$seg"`

// statuslineShadowNotice renders the warning for a user-scope waired segment
// that a higher-precedence statusLine shadows for the probed directory.
// Empty when nothing shadows or the detection failed (best-effort — the walk
// exists to warn, never to block).
func statuslineShadowNotice(eff claudecode.EffectiveStatusLine, err error) string {
	if err != nil || !eff.Shadowed() {
		return ""
	}
	return fmt.Sprintf("  note: this directory's Claude statusLine (%s, %s scope) takes precedence,\n"+
		"  so the waired segment will NOT be visible in sessions started here.\n"+
		"  waired never edits that file. To show routing there, append this line to your statusline script:\n"+
		"    %s\n", eff.Path, eff.Scope, statuslineSnippet)
}

// warnStatuslineShadow prints the shadow notice for the invoker's cwd after a
// statusline install. sudo preserves the caller's cwd, so this probes the
// directory the user actually ran enable from.
func warnStatuslineShadow(home string) {
	cwd, _ := os.Getwd()
	eff, err := claudecode.DetectEffectiveStatusLine(home, cwd, claudemanaged.Path())
	if notice := statuslineShadowNotice(eff, err); notice != "" {
		fmt.Print(notice)
	}
}

// invokerHome resolves the human user's home for a per-user ~/.claude edit. Under
// sudo it looks up SUDO_USER's home (so the edit lands with correct ownership via
// the hop); otherwise the current user's home (edited in-process).
func invokerHome() (home string, viaSudo bool, sudoUser string) {
	if u, isSudo := invokingSudoUser(); isSudo {
		if acct, err := osuser.Lookup(u); err == nil && acct.HomeDir != "" {
			return acct.HomeDir, true, u
		}
		return "", false, ""
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", false, ""
	}
	return h, false, ""
}

func stdinIsInteractive() bool { return isTerminal(os.Stdin) }

func promptYesNo(question string) bool {
	fmt.Printf("%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// --- small mgmt helpers ------------------------------------------------------

func mgmtURL(mgmt, path string) string {
	mgmt = strings.TrimRight(mgmt, "/")
	if !strings.HasPrefix(mgmt, "http://") && !strings.HasPrefix(mgmt, "https://") {
		mgmt = "http://" + mgmt
	}
	return mgmt + path
}

// fastGet is a short-timeout GET for the latency-sensitive statusline/hook. On
// any non-2xx or transport error it returns an error and the caller stays silent.
func fastGet(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
