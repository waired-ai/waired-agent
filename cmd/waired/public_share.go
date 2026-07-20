package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// errPublicShareUnsupported is returned when the daemon answers the
// provider Public Share toggle routes with 404 — an older waired-agent
// that predates the /waired/v1/public/share/{enable,disable} handlers.
// Unlike `waired public status` (which degrades each half independently),
// an explicit share/unshare has nothing to fall back to, so it surfaces
// this as a hard error telling the operator to upgrade.
var errPublicShareUnsupported = errors.New("daemon does not expose the public share toggle; upgrade waired-agent")

func newPublicShareCmd() *cobra.Command {
	var mgmt, stateDir string
	var maxClients int
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Share this computer publicly with other Waired users.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			maxClientsSet := cmd.Flags().Changed("max-clients")
			return runPublicShare(mgmt, stateDir, maxClients, maxClientsSet, os.Stdout)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to persist the public-share choice when the daemon is unreachable")
	cmd.Flags().IntVar(&maxClients, "max-clients", 0, "how many guests may use this computer at once (0 = the Waired default)")
	return cmd
}

func newPublicUnshareCmd() *cobra.Command {
	var mgmt, stateDir string
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "unshare",
		Short: "Stop sharing this computer publicly.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPublicUnshare(mgmt, stateDir, assumeYes, os.Stdout)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "where to persist the public-share choice when the daemon is unreachable")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// runPublicShare implements `waired public share`. It POSTs to the
// provider enable route, optionally carrying a --max-clients cap, and
// prints the server-authored result. When the daemon is unreachable it
// persists the desired choice so the next start applies it — the same
// dual-path pattern as `waired inference share on` (runShareTransition).
func runPublicShare(mgmt, stateDir string, maxClients int, maxClientsSet bool, out io.Writer) error {
	// Client-side guard: the offline fallback cannot round-trip a cap to
	// the control plane, and rejecting here avoids a pointless HTTP call.
	if maxClients < 0 {
		return errors.New("waired public share: --max-clients must be >= 0")
	}

	// The server's request struct is unexported; POST a nil body unless a
	// cap was set, matching the handler's tolerate-empty-body contract.
	var body any
	if maxClientsSet {
		type publicShareEnableBody struct {
			MaxClients int `json:"max_clients"`
		}
		body = publicShareEnableBody{MaxClients: maxClients}
	}

	var resp management.PublicShareStateResponse
	err := publicPostJSON(mgmt, "/waired/v1/public/share/enable", body, &resp)
	if err == nil {
		printPublicShareResult(out, resp)
		return nil
	}
	if isMgmtStatus(err, http.StatusNotFound) {
		return errPublicShareUnsupported
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired public share: %w", err)
	}

	// Daemon down: persist desired-on so the next start applies it.
	if writeErr := state.WriteDesiredPublicShare(stateDir, state.PublicShareOn); writeErr != nil {
		return fmt.Errorf("waired public share: daemon unreachable AND could not persist choice: %w", writeErr)
	}
	pln(out, "waired-agent not running — public share on persisted; will apply on next start.")
	if maxClientsSet {
		// There is no offline representation of the cap; do not invent one.
		pln(out, "--max-clients was not saved; set it again once waired-agent is running.")
	}
	return nil
}

// runPublicUnshare implements `waired public unshare`. Because disabling
// cuts off any public requests other people are running on this computer
// right now (spec §4.1 kill-switch), it pre-confirms on a TTY unless
// --yes was passed. It then POSTs the disable route and prints the
// server-authored result, falling back to persisting desired-off when the
// daemon is unreachable.
func runPublicUnshare(mgmt, stateDir string, assumeYes bool, out io.Writer) error {
	if !assumeYes {
		// confirmTTY appends " [y/N]: " itself and returns false on a
		// non-TTY / EOF, so a scripted caller without --yes safely aborts.
		if !confirmTTY("Stop public sharing? Any requests other people are running on this computer right now will be cut off.") {
			pln(out, "Cancelled.")
			return nil
		}
	}

	var resp management.PublicShareStateResponse
	err := publicPostJSON(mgmt, "/waired/v1/public/share/disable", nil, &resp)
	if err == nil {
		printPublicShareResult(out, resp)
		return nil
	}
	if isMgmtStatus(err, http.StatusNotFound) {
		return errPublicShareUnsupported
	}
	if !isConnectionRefused(err) {
		return fmt.Errorf("waired public unshare: %w", err)
	}

	if writeErr := state.WriteDesiredPublicShare(stateDir, state.PublicShareOff); writeErr != nil {
		return fmt.Errorf("waired public unshare: daemon unreachable AND could not persist choice: %w", writeErr)
	}
	pln(out, "waired-agent not running — public share off persisted; will apply on next start.")
	return nil
}

// printPublicShareResult renders a toggle response. The state line uses
// the shared renderPublicShareState mapping; RevokedGrants and Note are
// server-authored side-effect reporting — the Note (mesh auto-enable /
// disable / pending-sync wording) is printed VERBATIM, never re-authored
// here, so a single source drives every UI surface.
func printPublicShareResult(out io.Writer, r management.PublicShareStateResponse) {
	pf(out, "Sharing this computer: %s\n", renderPublicShareState(r.State))
	if r.RevokedGrants > 0 {
		pf(out, "Guest passes cancelled: %d\n", r.RevokedGrants)
	}
	if r.Note != "" {
		pln(out, r.Note)
	}
}
