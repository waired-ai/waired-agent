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
	var mgmt, sub string
	cmd := &cobra.Command{
		Use:   "route [auto|waired|anthropic]",
		Short: "Show or set where Claude Code runs (main conversation + subagents).",
		Long: `Choose where Claude Code's requests run, live — the next request honours it
with no Claude restart. The positional argument sets the MAIN conversation;
--subagents sets the subagent class independently.

  auto       Waired first; fall back to the real Anthropic API on failure (default)
  waired     Waired inference only; never contacts Anthropic
  anthropic  always the real Anthropic API (your Claude subscription)

  waired claude route                         show the current policy
  waired claude route auto                    main conversation → auto
  waired claude route anthropic --subagents waired   main → Anthropic, subagents stay on Waired
  waired claude route --subagents same        subagents follow the main conversation (default)

"waired" uses your Waired inference — WHICH node (this device or a mesh peer)
follows your 'waired worker' setting. Also available inside a Claude Code
session as the /waired-route slash command.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			subSet := cmd.Flags().Changed("subagents")
			if len(args) == 0 && !subSet {
				return runClaudeRoutingShow(mgmt)
			}
			var req management.ClaudeRoutingRequest
			if len(args) == 1 {
				m, err := normalizeMainRoute(args[0])
				if err != nil {
					return err
				}
				req.Main = &m
			}
			if subSet {
				sr, err := normalizeSubRoute(sub)
				if err != nil {
					return err
				}
				req.Sub = &sr
			}
			return postClaudeRouting(mgmt, req)
		},
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.Flags().StringVar(&sub, "subagents", "", "set the subagent class: same|auto|waired|anthropic")
	return cmd
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

func claudeServedDisplay(st management.ClaudeRoutingState) string {
	where := "this device"
	if st.LastServedBy != "" {
		where = "peer " + st.LastServedBy
	}
	if st.LastLocalModel == "" {
		return where
	}
	return fmt.Sprintf("%s (%s)", st.LastLocalModel, where)
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
		who := w.PinnedPeerName
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

func claudeRouteURL(mgmt string) string {
	mgmt = strings.TrimRight(mgmt, "/")
	if !strings.HasPrefix(mgmt, "http://") && !strings.HasPrefix(mgmt, "https://") {
		mgmt = "http://" + mgmt
	}
	return mgmt + "/waired/v1/integration/claude/route"
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
