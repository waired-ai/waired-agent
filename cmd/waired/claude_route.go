package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// newClaudeRouteCmd implements the unified `waired claude route
// [auto|waired|anthropic] [--subagents ...]`. It shows or sets the per-class
// routing policy the running agent consults per request, so the next Claude
// Code request honours it with no restart. The positional arg sets the main
// conversation; --subagents sets the subagent class. The /waired-route slash
// command shells out to exactly this.
func newClaudeRouteCmd() *cobra.Command {
	var mgmt, main, sub, subagents string
	cmd := &cobra.Command{
		Use:   "route [auto|waired|anthropic]",
		Short: "Show or set where Claude Code runs (main conversation + subagents).",
		Long: `Choose where Claude Code's requests run, live — the next request honours it
with no Claude restart.

The positional argument sets ALL of Claude Code: the main conversation moves,
and subagents go back to following it. --main and --sub each set one of them
and leave the other alone.

  auto       Waired first; fall back to the real Anthropic API on failure (default)
  waired     Waired inference only; never contacts Anthropic
  anthropic  always the real Anthropic API (your Claude subscription)

  waired claude route                          show the current policy
  waired claude route auto                     all of Claude Code → auto
  waired claude route --main anthropic         main conversation only
  waired claude route --sub waired             subagents only
  waired claude route anthropic --sub waired   main → Anthropic, subagents on Waired
  waired claude route --sub same               subagents follow the main conversation

"waired" uses your Waired inference — WHICH node (this device or a mesh peer)
follows your 'waired worker' setting. Also available inside a Claude Code
session as the /waired-route slash command.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := planClaudeRouteChange(claudeRouteArgs{
				positional:   args,
				main:         main,
				mainSet:      cmd.Flags().Changed("main"),
				sub:          sub,
				subSet:       cmd.Flags().Changed("sub"),
				subagents:    subagents,
				subagentsSet: cmd.Flags().Changed("subagents"),
			})
			if err != nil {
				return err
			}
			if plan.show {
				return runClaudeRoutingShow(mgmt)
			}
			return applyClaudeRouting(mgmt, plan)
		},
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.Flags().StringVar(&main, "main", "", "set only the main conversation: auto|waired|anthropic")
	cmd.Flags().StringVar(&sub, "sub", "", "set only the subagent class: same|auto|waired|anthropic")
	// The name this flag shipped under. Kept working so existing notes,
	// scripts and muscle memory do not break, and kept out of --help so
	// there is one name to learn.
	cmd.Flags().StringVar(&subagents, "subagents", "", "deprecated alias for --sub")
	_ = cmd.Flags().MarkHidden("subagents")
	return cmd
}

// claudeRouteArgs is everything the command line said, as facts.
type claudeRouteArgs struct {
	positional   []string
	main         string
	mainSet      bool
	sub          string
	subSet       bool
	subagents    string
	subagentsSet bool
}

// claudeRoutePlan is what to do about it. clearsPin records that the
// plan sets subagents back to "same" as a SIDE EFFECT of the positional
// argument rather than because the user asked for it — the one case the
// output has to explain.
type claudeRoutePlan struct {
	show      bool
	req       management.ClaudeRoutingRequest
	clearsPin bool
}

// planClaudeRouteChange turns the command line into one request
// (waired-agent#789).
//
// The rule is one line: the positional argument means "all of Claude
// Code", the flags mean "just this class".
//
// It used to be "the positional argument sets the main conversation, and
// --subagents sets subagents independently", which left `route auto` --
// a command that reads as "back to the defaults" -- with a subagent pin
// from an earlier command still in force. Subagent traffic then kept
// going somewhere the user had stopped asking for, including the
// waired-only route whose failure mode was a silent hang
// (waired-agent#788).
func planClaudeRouteChange(a claudeRouteArgs) (claudeRoutePlan, error) {
	sub, subSet := a.sub, a.subSet
	if a.subagentsSet {
		if subSet {
			return claudeRoutePlan{}, fmt.Errorf(
				"waired claude route: --sub and --subagents are the same flag; use --sub")
		}
		sub, subSet = a.subagents, true
	}
	if len(a.positional) == 1 && a.mainSet {
		return claudeRoutePlan{}, fmt.Errorf(
			"waired claude route: %q sets all of Claude Code and --main sets the main conversation; use one",
			a.positional[0])
	}
	if len(a.positional) == 0 && !a.mainSet && !subSet {
		return claudeRoutePlan{show: true}, nil
	}

	var plan claudeRoutePlan
	if subSet {
		sr, err := normalizeSubRoute(sub)
		if err != nil {
			return claudeRoutePlan{}, err
		}
		plan.req.Sub = &sr
	}
	switch {
	case len(a.positional) == 1:
		m, err := normalizeMainRoute(a.positional[0])
		if err != nil {
			return claudeRoutePlan{}, err
		}
		plan.req.Main = &m
		if !subSet {
			same := state.ClaudeRouteSame
			plan.req.Sub = &same
			plan.clearsPin = true
		}
	case a.mainSet:
		m, err := normalizeMainRoute(a.main)
		if err != nil {
			return claudeRoutePlan{}, err
		}
		plan.req.Main = &m
	}
	return plan, nil
}

// applyClaudeRouting sends the plan and prints the resulting policy.
//
// When the plan clears a subagent pin the user did not name, the pin is
// read BEFORE the change: after it, nothing on the wire says a pin was
// ever there, and silently dropping one is what waired-agent#789 is
// about. A failed read is not fatal — the routing change is what was
// asked for, and the note is an explanation of it.
func applyClaudeRouting(mgmt string, plan claudeRoutePlan) error {
	cleared := state.ClaudeRouteClass("")
	if plan.clearsPin {
		cleared = claudeSubPinBefore(mgmt)
	}
	if err := postClaudeRouting(mgmt, plan.req); err != nil {
		return err
	}
	if cleared != "" {
		fmt.Printf("%-20s%s\n", "", claudeSubPinClearedNote(cleared))
	}
	return nil
}

// claudeSubPinBefore reports the subagent pin currently in force, or ""
// when subagents already follow the main conversation (or the agent
// cannot be read).
func claudeSubPinBefore(mgmt string) state.ClaudeRouteClass {
	body, err := httpGet(claudeRouteURL(mgmt))
	if err != nil {
		return ""
	}
	var st management.ClaudeRoutingState
	if json.Unmarshal(body, &st) != nil {
		return ""
	}
	if st.Policy.Sub == "" || st.Policy.Sub == state.ClaudeRouteSame {
		return ""
	}
	return st.Policy.Sub
}

// claudeSubPinClearedNote explains a pin this command dropped.
func claudeSubPinClearedNote(was state.ClaudeRouteClass) string {
	return fmt.Sprintf("(subagents were pinned to %s — cleared. Pin them again with --sub %s)", was, was)
}

// normalizeMainRoute validates a main-class route, accepting "local" as a
// back-compat alias for "waired".
func normalizeMainRoute(arg string) (state.ClaudeRouteClass, error) {
	v := strings.ToLower(strings.TrimSpace(arg))
	if v == "local" {
		v = string(state.ClaudeRouteWaired)
	}
	switch state.ClaudeRouteClass(v) {
	case state.ClaudeRouteAuto, state.ClaudeRouteWaired, state.ClaudeRouteAnthropic:
		return state.ClaudeRouteClass(v), nil
	}
	return "", fmt.Errorf("waired claude route: unknown route %q (want auto|waired|anthropic)", arg)
}

// normalizeSubRoute validates a subagent-class route, additionally accepting
// "same" (inherit main).
func normalizeSubRoute(arg string) (state.ClaudeRouteClass, error) {
	v := strings.ToLower(strings.TrimSpace(arg))
	if v == string(state.ClaudeRouteSame) {
		return state.ClaudeRouteSame, nil
	}
	return normalizeMainRoute(arg)
}

func runClaudeRoutingShow(mgmt string) error {
	body, err := httpGet(claudeRouteURL(mgmt))
	if err != nil {
		return claudeRouteErr("route", mgmt, err)
	}
	return printClaudeRoutingState(mgmt, body)
}

func postClaudeRouting(mgmt string, req management.ClaudeRoutingRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("waired claude route: encode: %w", err)
	}
	body, err := httpPost(claudeRouteURL(mgmt), payload)
	if err != nil {
		return claudeRouteErr("route", mgmt, err)
	}
	return printClaudeRoutingState(mgmt, body)
}

func printClaudeRoutingState(mgmt string, body []byte) error {
	var st management.ClaudeRoutingState
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("waired claude route: parse: %w", err)
	}
	pol := st.Policy
	if pol.Main == "" {
		pol.Main = state.ClaudeRouteAuto
	}
	fmt.Printf("main conversation:  %s%s\n", pol.Main, claudeRouteHint(pol.Main))
	fmt.Printf("subagents:          %s\n", claudeSubDisplay(pol))
	// "waired" node follows the worker preference — surface it best-effort so
	// the user sees where local traffic lands without re-deriving it.
	if line := claudeWairedNodeLine(mgmt); line != "" {
		fmt.Printf("waired node:        %s\n", line)
	}
	if st.LastServedBy != "" || st.LastLocalModel != "" {
		fmt.Printf("last served:        %s\n", claudeServedDisplay(st))
	}
	if st.LastFallback != nil {
		fmt.Printf("last fallback:      %s\n", claudeFallbackDisplay(st.LastFallback))
	}
	return nil
}

// claudeRouteHintText is the bare one-line explanation for a route class,
// without surrounding punctuation, so callers can frame it themselves.
func claudeRouteHintText(r state.ClaudeRouteClass) string {
	switch r {
	case state.ClaudeRouteWaired:
		return "Waired only; never contacts Anthropic"
	case state.ClaudeRouteAnthropic:
		return "always the real Anthropic API"
	default:
		return "prefer Waired; visible fallback to Anthropic on failure"
	}
}

// claudeRouteHint annotates one route with a one-line explanation.
func claudeRouteHint(r state.ClaudeRouteClass) string {
	return "  (" + claudeRouteHintText(r) + ")"
}

// claudeSubDisplay renders the subagent class, spelling out what "same"
// resolves to.
func claudeSubDisplay(pol state.ClaudeRoutingPolicy) string {
	if pol.Sub == "" || pol.Sub == state.ClaudeRouteSame {
		eff := pol.Effective(state.ClaudeClassSub)
		return fmt.Sprintf("same as main  (%s — %s)", eff, claudeRouteHintText(eff))
	}
	return string(pol.Sub) + claudeRouteHint(pol.Sub)
}

// claudeServedDisplay renders the last request Waired served: what answered,
// where, and when. It leads with the time, like the last-fallback line, so the
// two read against each other — the served record is never cleared, so without
// a time one left over from before a fallback looks current (#755). The time
// is omitted when the agent did not report one, which is what an agent
// predating the field does.
func claudeServedDisplay(st management.ClaudeRoutingState) string {
	where := "this device"
	if st.LastServedBy != "" {
		where = "peer " + st.LastServedBy
	}
	served := where
	if st.LastLocalModel != "" {
		served = fmt.Sprintf("%s (%s)", st.LastLocalModel, where)
	}
	if st.LastServedAt.IsZero() {
		return served
	}
	return fmt.Sprintf("%s — %s", st.LastServedAt.Local().Format(time.RFC3339), served)
}

func claudeFallbackDisplay(e *management.ClaudeRoutingFallbackEvent) string {
	when := e.When.Local().Format(time.RFC3339)
	served := "Anthropic"
	if e.Direction == "local" {
		served = "locally"
	}
	class := e.Class
	if class == "" {
		class = "main"
	}
	return fmt.Sprintf("%s — %s served %s (%s), %d total", when, class, served, e.Reason, e.Count)
}

// claudeWairedNodeLine describes the node "waired" traffic would use, derived
// from the worker preference (GET /worker). Best-effort: an unreachable agent
// or old daemon yields "" (the line is skipped).
func claudeWairedNodeLine(mgmt string) string {
	body, err := httpGet(workerURL(mgmt))
	if err != nil {
		return ""
	}
	var w management.WorkerResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return ""
	}
	switch w.Mode {
	case state.RoutingModePinned:
		// Same identifier rule as `waired worker get`'s displayPin: the
		// daemon's display identifier, which is the grant pseudonym when
		// the pin is a public machine (#739, spec §8.5). The device-id
		// fallback is only for an agent predating that field.
		who := w.PinnedPeerName
		if who == "" {
			who = w.PinnedPeerDisplayID
		}
		if who == "" {
			who = w.PinnedPeerDeviceID
		}
		if who == "" {
			who = "(pinned peer)"
		}
		status := ""
		if w.PinnedPeerStatus != "" {
			status = " — " + w.PinnedPeerStatus
		}
		return fmt.Sprintf("pinned to %s%s   (change with `waired worker`)", who, status)
	case state.RoutingModeLocalOnly:
		return "this device only   (change with `waired worker`)"
	case state.RoutingModePeerPreferred:
		return "mesh (peer-preferred)   (change with `waired worker`)"
	case state.RoutingModePeerOnly:
		return "mesh only (peer-only)   (change with `waired worker`)"
	default:
		return "auto (this device or a mesh peer)   (change with `waired worker`)"
	}
}

// newClaudeRouteSkillCmd is the hidden `waired claude _route-skill
// <install|remove>` worker (#580). It (un)installs the /waired-route slash
// command into the CURRENT user's ~/.claude/skills/. `waired claude enable`
// invokes it via the sudo-user hop (runLinkAllAsUser) so, under elevation,
// the file lands in the invoking user's home with correct ownership; run
// directly it targets the current user. Hidden because users drive it
// through enable/disable, not by hand.
func newClaudeRouteSkillCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_route-skill <install|remove>",
		Short:  "internal: (un)install the /waired-route slash command for the current user",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _route-skill: resolve home: %w", err)
			}
			switch args[0] {
			case "install":
				if err := claudecode.InstallRouteSkill(home); err != nil {
					return err
				}
				fmt.Printf("Installed /waired-route slash command: %s\n", claudecode.SkillFile(home, claudecode.RouteSkillName))
				return nil
			case "remove":
				return claudecode.RemoveRouteSkill(home)
			default:
				return fmt.Errorf("waired claude _route-skill: unknown action %q (install|remove)", args[0])
			}
		},
	}
}

// installRouteSkillForInvoker / removeRouteSkillForInvoker (un)install the
// /waired-route slash command for the human user, hopping to them under
// sudo. Best-effort: a failure is warned, not fatal — the managed-settings
// write (the core of enable/disable) has already happened. The toggle can't
// help failure mode (a) (agent fully down), only give the user an in-session
// escape while the agent is up (#580).
func installRouteSkillForInvoker() { manageRouteSkillForInvoker("install") }
func removeRouteSkillForInvoker()  { manageRouteSkillForInvoker("remove") }

func manageRouteSkillForInvoker(action string) {
	if sudoUser, isSudo := invokingSudoUser(); isSudo {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runLinkAllAsUser(ctx, sudoUser, []string{"claude", "_route-skill", action}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s /waired-route slash command for user %q failed: %v\n", action, sudoUser, err)
		}
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot resolve home to %s /waired-route slash command: %v\n", action, err)
		return
	}
	switch action {
	case "install":
		if err := claudecode.InstallRouteSkill(home); err != nil {
			fmt.Fprintf(os.Stderr, "warning: install /waired-route slash command failed: %v\n", err)
			return
		}
		fmt.Printf("Installed /waired-route slash command: %s\n", claudecode.SkillFile(home, claudecode.RouteSkillName))
	case "remove":
		if err := claudecode.RemoveRouteSkill(home); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove /waired-route slash command failed: %v\n", err)
		}
	}
}

// claudeRoutePath is the management route carrying the Claude routing
// policy. Socket-only: it is not in the daemon's tcpReadRoutes allow-list
// (#785, waired#836).
const claudeRoutePath = "/waired/v1/integration/claude/route"

func claudeRouteURL(mgmt string) string {
	return mgmtURL(mgmt, claudeRoutePath)
}

// claudeRouteErr turns a bare transport failure into an actionable message:
// the route toggle needs a running agent. It cannot help failure mode (a)
// where the agent process itself is down (#580) — in that case Claude Code
// hits connection-refused directly and the user's recourse is to start the
// agent (or `waired claude disable`). httpGet/httpPost format daemon HTTP
// errors as "status N: ..."; anything else is a connectivity failure.
func claudeRouteErr(verb, mgmt string, err error) error {
	if !strings.HasPrefix(err.Error(), "status ") {
		return fmt.Errorf("waired claude %s: cannot reach the waired agent at %s (%v)\n"+
			"  the route toggle needs a running agent — start it and retry (see `waired claude status`)", verb, mgmt, err)
	}
	return fmt.Errorf("waired claude %s: %w", verb, err)
}
