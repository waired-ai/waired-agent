package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/deauth"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
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
	var stateDir string
	var yes, local, serverOnly, revoke bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove this device's identity + secrets so the next 'waired init' re-enrolls cleanly.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogoutBody(stateDir, yes, local, serverOnly, revoke)
		},
	}
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

func runLogoutBody(stateDir string, yes, local, serverOnly, revoke bool) error {
	if _, err := os.Stat(stateDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("logout: state directory does not exist; nothing to do.")
			return nil
		}
		return fmt.Errorf("stat %s: %w", stateDir, err)
	}

	if !yes {
		action := "deauthenticate and delete identity + secrets at " + stateDir
		if serverOnly {
			action = "deregister the device from the control plane (local identity + secrets kept)"
		}
		fmt.Printf("This will %s. Continue? [y/N] ", action)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errors.New("logout: aborted")
		}
	}

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

	// PathsUnder: logout only deletes; it must not create (or chmod) a
	// state dir on a never-enrolled machine.
	p, err := identity.PathsUnder(stateDir)
	if err != nil {
		return fmt.Errorf("paths: %w", err)
	}

	// Keychain-backed secrets (#261): delete from BOTH the macOS Keychain
	// and the file, so a stale Keychain item can't resurrect a logged-out
	// credential on the next read. p.RefreshToken is included here so
	// secrets/ is fully wiped — it was previously left behind.
	keychainTargets := []struct {
		item keychain.Item
		path string
	}{
		{keychain.Item{Account: securestore.Account, Service: securestore.ServiceMachineKey}, p.MachineKey},
		{keychain.Item{Account: securestore.Account, Service: securestore.ServiceAccessToken}, p.AccessToken},
		{keychain.Item{Account: securestore.Account, Service: securestore.ServiceRefreshToken}, p.RefreshToken},
	}
	for _, kt := range keychainTargets {
		if err := securestore.Remove(kt.item, kt.path); err != nil {
			return fmt.Errorf("remove %s: %w", kt.path, err)
		}
	}

	// File-only artifacts: identity.json (not a secret) and the node key
	// (file-only this round; Keychain backing is a #261 follow-up). cache/*
	// is left intact: NetworkMap and signing-key cache are recoverable from
	// the CP and harmless without secrets.
	for _, path := range []string{p.Identity, p.NodeKey} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	// Best-effort empty-dir prune for secrets/.
	if entries, err := os.ReadDir(filepath.Join(stateDir, "secrets")); err == nil && len(entries) == 0 {
		_ = os.Remove(filepath.Join(stateDir, "secrets"))
	}

	fmt.Println("logout: identity + secrets removed.")
	return nil
}

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
		fmt.Fprintf(os.Stderr,
			"logout: warning: could not %s with the control plane (%v).\n"+
				"        The device may still be active server-side; revoke it from the web admin if needed.\n", verb, err)
		return
	}
	if outcome != deauth.OutcomeDone {
		return // not enrolled / no token — nothing was attempted
	}
	if mode == deauth.ModeRevoke {
		fmt.Println("logout: device deregistered from the control plane.")
	} else {
		fmt.Println("logout: device deauthenticated server-side.")
	}
}
