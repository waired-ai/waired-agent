package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// This file holds what is left of `waired claude route` (#580): the readers
// the status line and `waired claude status` use to say what the Claude
// surface last did, and the worker that removes the retired /waired-route
// slash command from a user's home.
//
// The command itself is gone. It set a per-class route — auto / waired /
// anthropic, main and subagents — and there is no route left to set: a turn
// runs where its model id says, and waired holds none of its own that could
// send it elsewhere
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). Switching sides is `/model`.

// claudeServedDisplay renders the last-served record: which model answered,
// on which computer, and when.
func claudeServedDisplay(st management.ClaudeRoutingState, peerName string) string {
	where := "this device"
	if st.LastServedBy != "" {
		// waired-agent#1040: the name, when this device has one. The record
		// carries a DeviceID because that is what routing keys on, and a
		// person reading "which computer answered my turn" cannot act on an
		// identifier. The id remains the fallback for a machine that has
		// since left the mesh, which is still better than saying nothing.
		who := peerName
		if who == "" {
			who = st.LastServedBy
		}
		where = "peer " + who
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

// claudePeerNameLookup resolves a served-record DeviceID to the name the mesh
// shows for it, or "" when it cannot be resolved in time.
func claudePeerNameLookup(mgmt, deviceID string) string {
	if deviceID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudePeerNameBudget)
	defer cancel()
	snap, err := fetchMeshSnapshotCtx(ctx, mgmt)
	if err != nil || snap == nil {
		return ""
	}
	for _, p := range snap.Peers {
		if p.DeviceID != deviceID {
			continue
		}
		if name, ok := inferencemesh.PeerDisplayName(p); ok {
			return name
		}
		return ""
	}
	return ""
}

// claudePeerNameBudget bounds that lookup. `waired claude status` is a
// report a person is watching run, and a name is a nicety — so it waits about
// as long as the statusline does and then prints the identifier instead.
const claudePeerNameBudget = 500 * time.Millisecond

// claudeWairedNodeLine describes which Waired node answers a turn addressed
// to Waired: the `waired worker` routing preference, which is where node
// selection lives.
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

// newClaudeRouteSkillCmd is the hidden `waired claude _route-skill remove`
// worker. It removes the retired /waired-route slash command from the CURRENT
// user's ~/.claude/skills/. Both `waired claude enable` and `disable` invoke
// it via the sudo-user hop (runLinkAllAsUser) so, under elevation, the file
// in the invoking user's home is the one that goes — enable included, because
// a host upgrading from a build that installed the skill would otherwise keep
// a command that no longer does anything.
func newClaudeRouteSkillCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_route-skill remove",
		Short:  "Internal: remove the retired /waired-route slash command for this user",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if args[0] != "remove" {
				return fmt.Errorf("waired claude _route-skill: unknown action %q (want remove)", args[0])
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("waired claude _route-skill: resolve home: %w", err)
			}
			return claudecode.RemoveRouteSkill(home)
		},
	}
}

// removeRouteSkillForInvoker removes the retired slash command for the user
// who invoked the elevated command, hopping to them when this process is root
// via sudo. Best-effort: a leftover file is untidy, not broken.
func removeRouteSkillForInvoker() {
	sudoUser, isSudo := invokingSudoUser()
	if isSudo {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// ascii: a child process's streams. It is `waired` again, run as the
		// invoking user, and it folds its own output.
		if err := runLinkAllAsUser(ctx, sudoUser, []string{"claude", "_route-skill", "remove"}, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(stderr, "Warning: remove /waired-route for user %q failed: %v\n", sudoUser, err)
		}
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "Warning: remove /waired-route: couldn't find the home directory: %v\n", err)
		return
	}
	if err := claudecode.RemoveRouteSkill(home); err != nil {
		fmt.Fprintf(stderr, "Warning: remove /waired-route: %v\n", err)
	}
}

// claudeRoutePath is the daemon's record of what the Claude surface last did.
// The name is historical: the endpoint used to set the route as well.
const claudeRoutePath = "/waired/v1/integration/claude/route"

func claudeRouteURL(mgmt string) string {
	return mgmtURL(mgmt, claudeRoutePath)
}
