package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// `waired share` is the one sharing switch that lives on the computer
// (waired#1297, owner ruling 2026-08-30): whether it lends itself out at
// all. Off stops every kind of serving at once, and nothing in the web
// console can turn it back on.
//
// Who this computer is offered to — the rest of your own computers,
// people outside your account — is set in the console instead, which is
// why there is no `on`/`off` here for either of them. `status` reports
// what the console decided so one command answers the whole question.
const shareLong = `Control whether this computer lends itself out at all.

Turning sharing off stops every kind of serving straight away: the other
computers on your account stop being answered, and anyone using this
computer from outside your account is cut off mid-request.

Who this computer is offered to is set in the Waired console, not here.
This switch only decides whether any of it happens.`

func newShareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Turn sharing this computer on or off, and show what it is shared with.",
		Long:  shareLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(
		newShareTransitionCmd("on", state.SharingOn),
		newShareTransitionCmd("off", state.SharingOff),
		newShareStatusCmd(),
	)
	return cmd
}

func newShareTransitionCmd(verb string, target state.SharingState) *cobra.Command {
	var mgmt, stateDir string
	cmd := &cobra.Command{
		Use:   verb,
		Short: fmt.Sprintf("Turn sharing this computer %s.", verb),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShareTransition(mgmt, stateDir, target, "share "+verb)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to persist the choice when the daemon is unreachable")
	return cmd
}

func newShareStatusCmd() *cobra.Command {
	var mgmt string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether this computer is sharing, and who with.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runShareStatus(mgmt, jsonOut, stdout)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the raw status object as JSON")
	return cmd
}

// runShareTransition tries the daemon first and falls back to writing
// desired-sharing so the next start picks the choice up — the same
// dual-path pattern as pause/resume. A daemon that is not running is
// also a computer that is not serving anybody, so the fallback loses
// nothing but the acknowledgement.
func runShareTransition(mgmt, stateDir string, target state.SharingState, verb string) error {
	gf := globalFlags{Mgmt: mgmt, StateDir: stateDir}

	endpoint := "/waired/v1/sharing/enable"
	if target == state.SharingOff {
		endpoint = "/waired/v1/sharing/disable"
	}

	body, err := httpPost(gf.Mgmt+endpoint, nil)
	if err == nil {
		fmt.Fprintf(stdout, "%s ok.\n", verb)
		return prettyPrint(body)
	}

	if !isConnectionRefused(err) {
		return fmt.Errorf("waired %s: daemon returned: %w", verb, err)
	}
	if writeErr := state.WriteDesiredSharing(gf.StateDir, target); writeErr != nil {
		return fmt.Errorf("waired %s: daemon unreachable AND could not write desired-sharing: %w", verb, writeErr)
	}
	// Named address rather than the house "waired-agent not running" the
	// other fallbacks use. Connection-refused is also what a wrong --mgmt
	// or WAIRED_MGMT produces, and this is a kill switch: telling someone
	// their computer has stopped sharing while a daemon they did not
	// reach goes on serving is a different kind of wrong from a log level
	// that did not apply (waired#1305).
	fmt.Fprintf(stdout, "Could not reach waired-agent at %s — %s persisted; it applies on the next start.\n",
		gf.Mgmt, verb)
	return nil
}

func runShareStatus(mgmt string, jsonOut bool, out io.Writer) error {
	gf := globalFlags{Mgmt: mgmt}
	body, err := httpGet(gf.Mgmt + "/waired/v1/sharing")
	if err != nil {
		if isMgmtStatus(err, http.StatusNotFound) {
			pln(out, "Sharing: unsupported by this daemon (upgrade waired-agent)")
			return nil
		}
		return fmt.Errorf("waired share status: %w", err)
	}
	var s management.ShareStateResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("waired share status: parse: %w", err)
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}

	pf(out, "Sharing this computer: %s\n", shareOnOff(s.State))
	// The persisted choice and the live one differ while the app is
	// closed. Saying only "off" there would read as a setting the
	// operator made, and send them looking for the command that undoes
	// it.
	if s.Suspended {
		pln(out, "Paused because the Waired app is not running. It resumes when the app starts.")
	} else if s.DesiredState != "" && s.DesiredState != s.State {
		pf(out, "Saved choice: %s\n", shareOnOff(s.DesiredState))
	}
	pf(out, "Your other computers: %s\n", shareOnOff(s.MeshShare))
	pf(out, "People outside your account: %s\n", shareOnOff(s.PublicShare))
	if s.PublicMaxClients > 0 {
		pf(out, "Guest limit: %d at once\n", s.PublicMaxClients)
	}
	pln(out, "Who this computer is shared with is set in the Waired console.")
	return nil
}

// shareOnOff renders the wire values. Empty means the daemon has not
// heard from the console yet in this run, which is different from off —
// saying "off" there would report a setting nobody made.
func shareOnOff(v string) string {
	switch v {
	// SharingState and MeshShareState spell the same two words, which is
	// why one renderer covers both.
	case string(state.SharingOn):
		return "on"
	case string(state.SharingOff):
		return "off"
	case "":
		return "not known yet"
	default:
		return v + " (unrecognised — check daemon version)"
	}
}
