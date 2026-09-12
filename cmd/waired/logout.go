package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/deauth"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
)

// runLogout deauthenticates the device server-side, then deletes
// identity.json + everything under secrets/, leaving cache/ alone. Like
// `tailscale logout` it tells the control plane to deauth the device
// (#115): the agent moves to reauth_required, its tokens are revoked, and
// peers drop it from their maps, and the row is preserved. It's idempotent:
// a missing state dir or a state dir without an identity is treated as
// success. Designed to be invoked over `pkexec` from the tray, so it must
// not be interactive when --yes is set.
//
// The preserved row is NOT the same device again on the next `waired init`,
// and this used to say it was. The control plane matches an enrolling device
// on its machine key alone, and the deletion above includes secrets/machine.key
// — so a sign-out enrolls fresh next time, and the preserved row goes on
// holding the display name, which is why the new one comes back as
// "<hostname>-1". Measured on macOS against 0.0.3-rc6; the behaviour is the
// owner's call (2026-09-12: a sign-out is not a removal, and the row staying
// listed as signed out is what a person expects), so what changes here is only
// the claim. Coming back as the same device is waired-agent#1323.
//
// Two flags adapt it for the uninstaller (see runLogoutBody):
//
//	--revoke       terminal revoke (removed from the device list) instead
//	               of a recoverable logout.
//	--server-only  contact the CP but keep local files (dpkg/purge owns the
//	               local deletion on Linux).
func newLogoutCmd() *cobra.Command {
	var mgmt, stateDir string
	var yes, local, serverOnly, revoke bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Sign this computer out (removes its identity and secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogoutBody(mgmt, stateDir, yes, local, serverOnly, revoke)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json / secrets/* / cache/*")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"skip the interactive confirmation (required when invoked via pkexec)")
	cmd.Flags().BoolVar(&local, "local", false,
		"only remove local credentials; skip telling the control plane "+
			"(use when offline or the CP is unreachable)")
	cmd.Flags().BoolVar(&serverOnly, "server-only", false,
		"contact the control plane to deregister but keep local identity + "+
			"secrets (used by the uninstaller; dpkg/purge owns local deletion)")
	cmd.Flags().BoolVar(&revoke, "revoke", false,
		"revoke the device (removed from the device list) instead of a "+
			"recoverable logout; used at uninstall time")
	return cmd
}

func runLogoutBody(mgmt, stateDir string, yes, local, serverOnly, revoke bool) error {
	if _, err := os.Stat(stateDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stdout, "Nothing to do: the state directory doesn't exist.")
			return nil
		}
		return fmt.Errorf("stat %s: %w", stateDir, err)
	}

	if !yes {
		action := "sign this computer out and delete its identity and secrets at " + stateDir
		if serverOnly {
			action = "remove this device from the control plane (local identity and secrets kept)"
		}
		fmt.Fprintf(stdout, "This will %s. Continue? [y/N] ", action)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errors.New("aborted, no changes made")
		}
	}

	// Hand the whole job to the running daemon when there is one. It owns the
	// state dir and the goroutines writing into it, so it is the only process
	// that can stop them before the files go away — and, decisively, the only
	// one that can stop SERVING the identity it just deleted. Doing this from
	// out here left the daemon answering as enrolled until its access token
	// lapsed or someone restarted it, and let a subsequent sign-in write
	// identity.json back from the live session (waired-agent#800, #1269).
	//
	// Not for --server-only: that mode deliberately keeps the local files
	// (the deb's prerm uses it so dpkg stays the single owner of deletion),
	// and the daemon route always removes them.
	if !serverOnly {
		if done, err := logoutViaDaemon(mgmt, local, revoke); done {
			if err == nil {
				afterSignOut(revoke)
			}
			return err
		}
	}

	// No daemon answered — an uninstall that already stopped the service, a
	// per-user install with nothing running, or an agent too old to offer the
	// route. Do it from here, as this command always did.

	// Best-effort server-side deauth/revoke BEFORE wiping the access token
	// from disk. A failure here never blocks the local wipe — logout must
	// always clear local state — but we warn loudly so the user knows the
	// device may still be active server-side.
	if !local {
		mode := deauth.ModeLogout
		if revoke {
			mode = deauth.ModeRevoke
		}
		deauthOnLogout(stateDir, mode)
	}

	// --server-only: the CP call is the whole job; leave local files intact.
	// The .deb prerm uses this so dpkg (remove keeps state, purge wipes it)
	// stays the single owner of local deletion on Linux.
	if serverOnly {
		return nil
	}

	// The path list and the empty-secrets/ prune live in
	// identity.RemoveEnrollment, which the daemon's own sign-out calls too. A
	// list that has already been corrected twice (#261 added the refresh
	// token after finding it survived a logout; waired#1277 removed the
	// gateway token with the credential) is a list to keep in one place.
	if err := identity.RemoveEnrollment(stateDir); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "Signed out. Identity and secrets removed.")
	afterSignOut(revoke)
	return nil
}

// afterSignOut clears the /model rows this user was offered and says what a
// sign-out deliberately leaves behind.
//
// The rows live in the user's own ~/.claude/settings.json (waired-agent#1185),
// so removing them needs no elevation — which matters, because sign-out asks
// for none (docs/decisions/20260907/0230-sign-out-is-the-daemons-job.md) and
// the machine-wide managed settings therefore stay exactly as they were. That
// is the split this prints: Claude Code goes on pointing at the local gateway,
// which is fine — an Anthropic model id passes through it to the real API as
// before, and a Waired id now fails with the reason rather than being answered
// by a computer that is signed out (waired-agent#1310).
//
// Best-effort, like every other per-user extra: a sign-out that removed the
// identity has done the thing it was asked to do.
func afterSignOut(revoke bool) {
	removePickerRowsForInvoker()
	removeModelDefaultForInvoker()
	fmt.Fprintln(stdout, "Waired's /model rows are gone from Claude Code. The Claude Code integration itself stays "+
		"(changing it needs administrator rights); `waired init` signs back in, and `waired claude disable` removes it.")
	if !revoke {
		fmt.Fprintln(stdout, "This computer stays in your device list as signed out. Remove it there if you don't plan to sign back in.")
	}
}

// logoutViaDaemon asks the running daemon to sign this device out. It reports
// whether it handled the job, so the caller can fall through to doing it
// in-process when no daemon answered.
//
// Falls through on exactly two conditions, and on nothing else:
//
//   - the daemon is not reachable (an uninstall that already stopped the
//     service, a machine with nothing running),
//   - the daemon answered 404, i.e. it predates the route.
//
// Every other answer is the daemon having an opinion. A 409 means a sign-in is
// in flight and the right thing is to say so and stop, not to race it by
// deleting the files out from under it; a 500 is a refusal, and retrying the
// same work from here would be doing what the daemon just declined to do.
func logoutViaDaemon(mgmt string, local, revoke bool) (handled bool, err error) {
	body, merr := json.Marshal(management.LogoutRequest{Revoke: revoke, SkipDeauth: local})
	if merr != nil {
		return false, merr
	}
	raw, perr := httpPostWithin(mgmt+"/waired/v1/logout", body, logoutDaemonBudget)
	if perr != nil {
		if isConnectionRefused(perr) {
			return false, nil
		}
		var se *mgmtStatusError
		if errors.As(perr, &se) && se.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return true, fmt.Errorf("waired logout: %w", perr)
	}
	var resp management.LogoutResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		// The sign-out happened; only the report did not parse.
		fmt.Fprintln(stdout, "Signed out. Identity and secrets removed.")
		return true, nil
	}
	if resp.DeauthError != "" {
		fmt.Fprintf(stderr, "Warning: couldn't reach the control plane (%s).\n"+
			"        This device may still be listed there. Remove it in the Waired console if needed.\n",
			resp.DeauthError)
	} else if resp.Deauthed {
		if revoke {
			fmt.Fprintln(stdout, "Removed this device from the control plane.")
		} else {
			fmt.Fprintln(stdout, "Signed out on the control plane.")
		}
	}
	fmt.Fprintln(stdout, "Signed out. Identity and secrets removed.")
	fmt.Fprintln(stdout, "The background service stopped serving this sign-in.")
	return true, nil
}

// logoutDaemonBudget outlasts the daemon's own control-plane call
// (deauthTimeout in cmd/waired-agent) plus the session teardown it then runs,
// so a slow control plane shows up as the daemon's warning rather than as this
// command timing out and reporting a sign-out that actually succeeded.
const logoutDaemonBudget = 30 * time.Second

// deauthOnLogout makes a best-effort server-side deauth (ModeLogout) or
// revoke (ModeRevoke) using the credentials still on disk, delegating to
// the shared internal/deauth helper (also used by `waired-agent
// uninstall`). It is intentionally non-fatal: any problem (not enrolled, no
// token, CP unreachable) is reported as a warning and the caller proceeds
// to wipe local state regardless.
func deauthOnLogout(stateDir string, mode deauth.Mode) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome, err := deauth.Deregister(ctx, stateDir, mode)
	if err != nil {
		verb := "deauthenticate"
		if mode == deauth.ModeRevoke {
			verb = "deregister"
		}
		fmt.Fprintf(stderr, "Warning: couldn't %s with the control plane (%v).\n"+
			"        This device may still be listed there. Remove it in the Waired console if needed.\n", verb, err)
		return
	}
	if outcome != deauth.OutcomeDone {
		return // not enrolled / no token — nothing was attempted
	}
	if mode == deauth.ModeRevoke {
		fmt.Fprintln(stdout, "Removed this device from the control plane.")
	} else {
		fmt.Fprintln(stdout, "Signed out on the control plane.")
	}
}
