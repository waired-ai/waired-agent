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
// peers drop it from their maps — the row is preserved, recoverable via
// `waired init`. It's idempotent: a missing state dir or a state dir
// without an identity is treated as success. Designed to be invoked over
// `pkexec` from the tray, so it must not be interactive when --yes is set.
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
		Short: "Remove this device's identity + secrets so the next 'waired init' re-enrolls cleanly.",
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
		"only wipe local credentials; skip the control-plane deauth call "+
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
			fmt.Fprintln(stdout, "logout: state directory does not exist; nothing to do.")
			return nil
		}
		return fmt.Errorf("stat %s: %w", stateDir, err)
	}

	if !yes {
		action := "deauthenticate and delete identity + secrets at " + stateDir
		if serverOnly {
			action = "deregister the device from the control plane (local identity + secrets kept)"
		}
		fmt.Fprintf(stdout, "This will %s. Continue? [y/N] ", action)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errors.New("logout: aborted")
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

	fmt.Fprintln(stdout, "logout: identity + secrets removed.")
	return nil
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
		fmt.Fprintln(stdout, "logout: identity + secrets removed.")
		return true, nil
	}
	if resp.DeauthError != "" {
		fmt.Fprintf(stderr, "logout: warning: could not reach the control plane (%s).\n"+
			"        The device may still be active server-side; revoke it from the web admin if needed.\n",
			resp.DeauthError)
	} else if resp.Deauthed {
		if revoke {
			fmt.Fprintln(stdout, "logout: device deregistered from the control plane.")
		} else {
			fmt.Fprintln(stdout, "logout: device deauthenticated server-side.")
		}
	}
	fmt.Fprintln(stdout, "logout: identity + secrets removed.")
	fmt.Fprintln(stdout, "logout: the background service stopped serving this sign-in.")
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
		fmt.Fprintf(stderr, "logout: warning: could not %s with the control plane (%v).\n"+
			"        The device may still be active server-side; revoke it from the web admin if needed.\n", verb, err)
		return
	}
	if outcome != deauth.OutcomeDone {
		return // not enrolled / no token — nothing was attempted
	}
	if mode == deauth.ModeRevoke {
		fmt.Fprintln(stdout, "logout: device deregistered from the control plane.")
	} else {
		fmt.Fprintln(stdout, "logout: device deauthenticated server-side.")
	}
}
