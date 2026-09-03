package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osuser "os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
	"github.com/waired-ai/waired-agent/internal/management"
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
		RunE: func(cmd *cobra.Command, _ []string) error { return runClaudeStatusline(mgmt, cmd.InOrStdin()) },
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
			fmt.Fprintf(stdout, "Removed waired routing statusline from %s\n", claudecode.SettingsPath(home))
			return nil
		},
	}
}

// runClaudeStatusline prints the footer segment. It prints NOTHING (a blank
// segment) unless waired currently owns the Claude route, and never returns an
// error to stdout — any failure degrades to blank or an "agent down" note.
func runClaudeStatusline(mgmt string, stdin io.Reader) error {
	_, present, baseURL := claudemanaged.View()
	if !present || !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		return nil // waired isn't routing Claude Code → blank segment
	}
	sessionModel := statuslineSessionModel(stdin)
	route, health, resident, mesh, ok := fetchRouteAndHealth(mgmt)
	if !ok {
		// ascii: this stdout is a pipe into Claude Code, which draws the
		// segment in its own UTF-8 UI. It is also exactly where foldOutput()
		// reports true on Windows — a pipe is not a TTY — so folding here
		// would degrade the one surface that renders these glyphs correctly.
		// slGlyph carries the ASCII fallback for the sinks that need one.
		fmt.Print(statuslineDown())
		return nil
	}
	// ascii: the same pipe into Claude Code's own UTF-8 UI as above.
	fmt.Print(renderStatusline(route, health, resident, mesh, sessionModel))
	return nil
}

// statuslineSessionModel reads the model id Claude Code selected for THIS
// session out of the payload it writes to the command's stdin. Everything about
// it is best-effort: a payload that is absent, truncated, or shaped differently
// yields "", and sessionSide reads that as Waired — the side an unnamed turn is
// in fact served on.
//
// Two guards, both about not hanging a footer:
//
//   - a terminal on stdin means a person is previewing the segment by hand, and
//     there is no payload coming. Reading would block until they hit ctrl-D.
//   - the read is capped. The footer has a budget measured in milliseconds and
//     the value needed is a short string near the top of the document.
func statuslineSessionModel(stdin io.Reader) string {
	if stdin == nil {
		return ""
	}
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		return ""
	}
	var payload struct {
		Model struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	if json.NewDecoder(io.LimitReader(stdin, statuslinePayloadCap)).Decode(&payload) != nil {
		return ""
	}
	return payload.Model.ID
}

// statuslinePayloadCap bounds the statusline payload read. Claude Code's
// document is a few hundred bytes of session metadata; this is generous enough
// to survive it growing and small enough that a wedged writer cannot stall the
// footer.
const statuslinePayloadCap = 64 << 10

// fetchRouteAndHealth queries the route state (required), inference health
// and the mesh (both best-effort) concurrently within the statusline budget.
// ok=false means the agent is unreachable.
//
// resident is nil when nothing claimed it: an older agent, a host with no
// ollama, or a reading never taken. It must never be read as "cold".
//
// The mesh read is a third concurrent call rather than a third fact folded
// into one of the other two, because the footer needs a different question
// answered than either of those endpoints was built for — see wairedTargets.
// It is concurrent, so it costs the segment no extra latency inside
// statuslineBudget, and a failure leaves the mesh unknown rather than empty
// (mesh.known), which is what keeps the line failing open onto the behaviour
// that shipped.
func fetchRouteAndHealth(mgmt string) (route management.ClaudeRoutingState, health string, resident *bool, mesh meshView, ok bool) {
	var wg sync.WaitGroup
	wg.Add(3)
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
				Runtimes       map[string]struct {
					ModelResidentIsActive *bool `json:"model_resident_is_active"`
				} `json:"runtimes"`
			}
			if json.Unmarshal(b, &h) == nil {
				health = h.SubsystemState
				// waired-agent#837. Widening the body this call already
				// fetches, so the extra fact costs no round trip inside
				// statuslineBudget. Only ollama has a residency axis; a
				// host serving on vLLM leaves this nil, which renders as
				// no claim.
				if ol, found := h.Runtimes["ollama"]; found {
					resident = ol.ModelResidentIsActive
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), statuslineBudget)
		defer cancel()
		snap, err := fetchMeshSnapshotCtx(ctx, mgmt)
		if err != nil || snap == nil {
			return
		}
		mesh = meshViewOf(snap)
	}()
	wg.Wait()
	return route, health, resident, mesh, ok
}

// meshView is what the footer needs to know about the OTHER computers: can
// any of them take the next turn, and what is the one that took the last one
// called (waired-agent#1042).
//
// known=false means the mesh could not be read — an older agent with no such
// route, a socket that did not answer inside the budget — and is NOT the same
// as "no peers". Every branch below treats it as "no claim" and renders what
// it rendered before the mesh was consulted at all.
type meshView struct {
	known     bool
	reachable bool
	// names maps a peer DeviceID to what a person should see. Public Share
	// machines resolve to their grant pseudonym, never the real device id
	// (spec §8.5), because inferencemesh.PeerDisplayName decides it.
	names map[string]string
}

func meshViewOf(snap *inferencemesh.Snapshot) meshView {
	v := meshView{known: true, reachable: snap.Reachable, names: map[string]string{}}
	for _, p := range snap.Peers {
		if name, ok := inferencemesh.PeerDisplayName(p); ok {
			v.names[p.DeviceID] = name
		}
	}
	return v
}

// peerName renders the machine a DeviceID belongs to, or "" when this device
// has no name for it — a peer that has since left the mesh, or a mesh read
// that did not come back. The caller says "a peer" in that case rather than
// printing an identifier nobody can act on.
func (v meshView) peerName(deviceID string) string {
	if deviceID == "" {
		return ""
	}
	return v.names[deviceID]
}

// renderStatusline builds the colored one-liner. Color is forced (Claude Code
// renders ANSI even though our stdout is a pipe) and gated only on NO_COLOR;
// the glyph degrades to ASCII under WAIRED_NO_EMOJI / a non-UTF-8 locale.
//
// The line answers one question — where does the next turn go — and until
// waired-agent#1042 it answered it from ONE input: whether this computer's own
// engine was ready. On a device deliberately without an engine, whose whole
// role is to borrow another computer's, that made it announce
// "fallback → Anthropic (local disabled)" before any turn had run, on a host
// where peers were in fact serving the turns (47 s and 171 s, no fallback
// recorded). The daemon's own routing decision has consulted both axes since
// waired-agent#829; this is the display catching up.
//
// So "is Waired serving" is now local OR peer, and where the two differ the
// line says which. A mesh that could not be read (mesh.known false) claims
// nothing and the line renders as it did before.
//
// sessionModel is the second half of the same correction, from the other
// direction (waired-agent#1037): the route came from a machine-wide setting
// while the line hangs under ONE session, so a session that picked Opus was
// told it was on Waired, and two sessions side by side were told the same
// thing when they were not.
func renderStatusline(route management.ClaudeRoutingState, health string, resident *bool, mesh meshView, sessionModel string) string {
	arrow := slGlyph("→", "->")
	// model is appended only on the branches that are actively serving on
	// Waired — an Anthropic or unavailable segment showing a local model id
	// would misread as "that model answered" (#602).
	model := ""
	if route.LastLocalModel != "" {
		model = " (" + route.LastLocalModel + ")"
	}
	// An unreadable inference status has always counted as ready: the line
	// must not report a fault it did not observe.
	localReady := health == "" || health == "ready"
	peerReady := mesh.known && mesh.reachable
	// The peer half of "on Waired": the machine, and what ran on it. Named
	// when this device knows which machine answered last, anonymous when it
	// does not — naming the one that WOULD answer next would mean running the
	// selector on every transcript update, which is not a thing a footer may
	// cost.
	//
	// The model is LastLocalModel, exactly as on the local branch: the gateway
	// stamps X-Waired-Local-Model for whichever node answered, mesh leg
	// included, so on this branch the pair is one turn's record and cannot
	// pair a machine with a model that ran somewhere else. Owner ruling
	// 2026-08-28: show both.
	peerLabel := " (peer)"
	if name := mesh.peerName(route.LastServedBy); name != "" {
		peerLabel = " (peer " + name + ")"
		if route.LastLocalModel != "" {
			peerLabel = " (peer " + name + ": " + route.LastLocalModel + ")"
		}
	}
	var glyph, label, color string
	switch {
	case sessionSide(sessionModel) == claudecode.RouteAnthropic:
		glyph, label, color = arrow, "waired: Anthropic", ansiYellow
	case localReady:
		glyph, label, color = slGlyph("⚡", ""), "waired: on Waired"+model, ansiGreen
	case peerReady:
		// A host with no engine of its own is not "down" while a peer can
		// answer — it is doing exactly what it was set up to do.
		glyph, label, color = slGlyph("⚡", ""), "waired: on Waired"+peerLabel, ansiGreen
	default:
		// Nothing on Waired can take the next turn, and nothing will carry it
		// to Anthropic either: the turn will fail with a reason
		// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
		// Red, because this is the state a user has to act on — by fixing the
		// engine, or by picking an Anthropic model in /model.
		glyph, label, color = slGlyph("⚠", "!"),
			"waired: Waired cannot answer ("+noWairedTargetReason(health, mesh)+")", ansiRed
	}
	seg := label
	if glyph != "" {
		seg = glyph + " " + label
	}
	seg += notLoadedSuffix(color, localReady, route, resident)
	return slSgr(color, seg)
}

// noWairedTargetReason says why nothing on Waired can take the next turn.
//
// It replaces the bare "local <state>" this branch used to print, which named
// one of the two axes and read as a whole answer — the defect waired-agent#1042
// was filed for. The local half is kept because it is the actionable one (a
// starting engine resolves itself, a disabled one does not), and the peer half
// is added because on an engine-less host it is the entire story.
//
// A mesh nobody could read is not reported as "no peer": the line says only
// what it observed, and falls back to the shipped wording.
func noWairedTargetReason(health string, mesh meshView) string {
	local := "local " + health
	if health == "" {
		local = "local unavailable"
	}
	if !mesh.known {
		return local
	}
	return local + ", no peer"
}

// sessionSide is where THIS session's next turn runs: the side the model id
// Claude Code selected names, and Waired when it names neither.
//
// The footer hangs under ONE session, so the id is the only honest input it
// has: a session that picked Opus is on the real Anthropic API, and a session
// running beside it on a Waired row is not (waired-agent#1037). Nothing is
// persisted from here. The id arrives on stdin, per render, from the session
// it describes.
func sessionSide(sessionModel string) string {
	if route, forced := claudecode.RouteForModelID(sessionModel); forced {
		return route
	}
	return claudecode.RouteWaired
}

// notLoadedSuffix says that the weights this computer serves are not in
// memory, on the branches where this computer is the one about to answer
// (waired-agent#837).
//
// It is here because of when Claude Code runs the statusline command: at
// transcript updates, which includes the user's own submission. The string
// computed at that moment then stays on screen for the whole turn — so a
// footer that already says "model not loaded" when the silence begins has
// answered "is this thing hung?" before it was asked. It does not need to
// re-render mid-wait, and it does not.
//
// Four conditions, and each removes a way to be wrong:
//
//   - green branches only. Yellow already means "not answered by Waired" on
//     this line, and the clause would be about a computer that is not
//     answering this turn.
//   - this computer is the one about to answer. Since waired-agent#1042 a
//     green branch can be green BECAUSE a peer is serving, and the weights on
//     this machine say nothing about that turn — on an engine-less host they
//     are never loaded, so without this the fixed line would have gained a
//     permanent "model not loaded".
//   - resident != nil. An older agent, a vLLM host, or a reading never taken
//     all leave it nil, and "we did not look" must not render as "cold".
//   - nothing served by a peer recently. LastServedBy names another machine,
//     whose weights this says nothing about; asserting local residency there
//     would be a fresh lie rather than a missing fact.
//
// The colour does not change. This is a fact about the next few seconds, not
// a fault, and the residency default is to hold the model — so the state
// this describes is the ordinary one right after a boot or a switch.
func notLoadedSuffix(color string, localReady bool, route management.ClaudeRoutingState, resident *bool) string {
	if color != ansiGreen || !localReady || resident == nil || *resident {
		return ""
	}
	if route.LastServedBy != "" {
		return ""
	}
	return slGlyph(" · ", " - ") + "model not loaded"
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

// slGlyph skips the TTY gate (stdout is the pipe to Claude Code) but otherwise
// shares the glyph decision, so a Windows console that carries UTF-8 gets the
// same statusline the other two OSes do.
func slGlyph(emoji, ascii string) string {
	if !glyphsSupported(runtime.GOOS, currentGlyphFacts()) {
		return ascii
	}
	return emoji
}

// --- enable/disable wiring (invoking-user hop) -------------------------------

// installStatuslineForInvoker is called by `waired claude enable` and by
// `waired init`. It classifies the invoking user's statusLine and installs
// waired's segment — prompting first when a foreign statusLine would have to
// be wrapped. allowPrompt=false (init --non-interactive) never asks even on a
// TTY: a foreign statusLine gets guidance instead of a blocking y/N read.
// Best-effort: warnings, not failures (the managed-settings write is the core
// of the caller).
//
// in is the caller's line source. `waired init` hands its stdin owner down
// so this question reads from the same queue as every other prompt of the
// run: a reader of our own here would sit behind the owner and never see
// its answer (#223).
func installStatuslineForInvoker(skip, allowPrompt bool, in lineReader) {
	if skip {
		return
	}
	home, viaSudo, sudoUser := invokerHome()
	if home == "" {
		fmt.Fprintln(stderr, "warning: cannot resolve invoking user's home for statusline install")
		return
	}
	kind, existing, err := claudecode.DetectStatusLine(home)
	if err != nil {
		fmt.Fprintf(stderr, "warning: reading %s: %v\n", claudecode.SettingsPath(home), err)
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
		if promptYesNo(q, in) {
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
		// ascii: a child process's streams. It is `waired` again, run as the
		// invoking user, and it folds its own output.
		if err := runLinkAllAsUser(ctx, sudoUser, []string{"claude", "statusline", "remove"}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(stderr, "warning: removing waired statusline for user %q failed: %v\n", sudoUser, err)
		}
		return
	}
	if err := claudecode.RemoveStatusLine(home); err != nil {
		fmt.Fprintf(stderr, "warning: removing waired statusline failed: %v\n", err)
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
		// ascii: a child process's streams. It is `waired` again, run as the
		// invoking user, and it folds its own output.
		if err := runLinkAllAsUser(ctx, sudoUser, args, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(stderr, "warning: installing waired statusline for user %q failed: %v\n", sudoUser, err)
		}
		return
	}
	res, err := claudecode.InstallStatusLine(home, wrap)
	if err != nil {
		fmt.Fprintf(stderr, "warning: installing waired statusline failed: %v\n", err)
		return
	}
	printStatuslineResult(res)
}

func printStatuslineResult(res claudecode.StatusLineResult) {
	switch res.Action {
	case "injected":
		fmt.Fprintf(stdout, "  Installed waired routing statusline in %s (restart the Claude session to see it).\n", res.Path)
		fmt.Fprintln(stdout, "  Note: a project-level statusLine (.claude/settings.local.json / settings.json) takes precedence over it — `waired claude status` run inside a project reports shadowing.")
	case "refreshed":
		fmt.Fprintf(stdout, "  waired routing statusline present in %s.\n", res.Path)
	case "wrapped":
		fmt.Fprintf(stdout, "  Wrapped your existing statusLine in %s (restored on `waired claude disable`).\n", res.Path)
	case "rewrapped":
		// waired-agent#787: the wrapper this host had was written for another
		// OS's shell — say what changed, since nothing else about the statusLine
		// looks different afterwards.
		fmt.Fprintf(stdout, "  Rewrote the waired statusline wrapper in %s for this computer's shell.\n", res.Path)
	case "already-wrapped":
		fmt.Fprintln(stdout, "  waired routing statusline already active.")
	case "skipped-foreign":
		printStatuslineGuidance(res.Existing)
	}
}

func printStatuslineGuidance(existing string) {
	fmt.Fprintf(stdout, "  You already have a Claude Code statusLine (%s); left unchanged.\n", existing)
	fmt.Fprintln(stdout, "  To also show waired routing, run: waired claude statusline install --wrap")
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
		fmt.Fprint(stdout, notice)
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

func promptYesNo(question string, in lineReader) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	if !in.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(in.Text())) {
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
//
// It routes through mgmtReadRoute — the socket, with a loopback-TCP fallback.
// Its own client read raw TCP, and /waired/v1/integration/claude/route is not
// in the daemon's tcpReadRoutes allow-list, so the daemon answered 403 and the
// statusline rendered "waired: agent down" on every turn of a healthy machine
// while `waired claude status` (which uses the routed httpGet) reported
// everything correct. The fallback hook, the other caller, stayed silent for
// the same reason (#785, waired#836).
func fastGet(url string, timeout time.Duration) ([]byte, error) {
	target, client, err := mgmtReadRoute(url, timeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Get(target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
