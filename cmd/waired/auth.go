package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// runAuth dispatches `waired auth <subcommand>` (spec §14.2). There is
// deliberately no `renew` sub-verb — rerunning `waired init` while
// already enrolled handles OAuth + Device-row update in one step
// (spec §14.3, #115 Phase C). The legacy `renew` keyword is kept as a
// discoverable shim that points operators at the consolidated path.
const authLong = `Inspect / manage device authentication.

  waired auth status   Show token state + expiry, suggest re-init
  waired auth logout   Alias for 'waired logout'

To re-authenticate (refresh tokens + Device Certificate) run 'waired init'
on an already-enrolled device.`

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Inspect / manage device authentication (status / logout).",
		Long:  authLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newAuthStatusCmd(), newLogoutCmd(), newAuthRenewCmd())
	return cmd
}

// newAuthRenewCmd is a discoverable shim: there is deliberately no `renew`
// sub-verb — rerunning `waired init` while already enrolled handles OAuth +
// Device-row update in one step. Hidden so it doesn't clutter help, but
// still pointable so operators who type it get the redirect.
func newAuthRenewCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "renew",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(os.Stderr,
				"`waired auth renew` was consolidated into `waired init`.")
			fmt.Fprintln(os.Stderr,
				"Just run `waired init` on an already-enrolled device to re-authenticate.")
			return errors.New("waired auth renew: use `waired init`")
		},
	}
}

// newAuthStatusCmd reads identity + token metadata from disk and prints a
// human-readable summary. Format mirrors spec §14.2 example output.
func newAuthStatusCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print account / device identity + token expiry (reads disk only).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthStatusBody(stateDir)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "",
		"override the agent state dir (default: system dir as root, per-user dir otherwise)")
	return cmd
}

func runAuthStatusBody(stateDirVal string) error {
	stateDir := &stateDirVal
	dir := *stateDir
	if dir == "" {
		// Mirror `waired status` / `waired init`: root on a service
		// install must read the SYSTEM dir, not /root/.config/waired —
		// the Interactive default here made even `sudo waired auth
		// status` claim "not enrolled" on enrolled machines.
		dir = defaultStateDir()
	}

	id, err := identity.Load(dir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("permission denied reading state in %s — %s",
				dir, elevationHint("waired auth status"))
		}
		return fmt.Errorf("auth status: %w", err)
	}
	if id == nil {
		if n := systemStateNotice(dir, "waired auth status"); n != "" {
			return errors.New(n)
		}
		fmt.Println("Waired: not enrolled. Run `waired init` to sign in.")
		return nil
	}
	meta, err := identity.LoadTokenMeta(dir)
	if err != nil {
		return fmt.Errorf("auth status: load token meta: %w", err)
	}
	access, _ := identity.LoadAccessToken(dir)
	refresh, _ := identity.LoadRefreshToken(dir)

	fmt.Printf("Account: %s\n", id.AccountEmail)
	fmt.Printf("Device:  %s\n", displayDeviceName(id))
	fmt.Printf("Network: %s\n", id.NetworkName)

	now := time.Now()
	switch {
	case meta.NeedsReauth():
		// reauth_required preempts the token-state lines below — telling
		// the operator "expires in 12 days" while the daemon has already
		// given up trying to refresh would be misleading.
		since := now.Sub(meta.ReauthRequiredAt).Round(time.Second)
		fmt.Printf("Auth:    REAUTH REQUIRED — flagged %s ago. Run `waired init` to recover.\n", since)
	case access == "":
		fmt.Println("Auth:    no access token — run `waired init`")
	case meta.AccessExpiresAt.IsZero():
		fmt.Println("Auth:    valid (no expiry recorded — pre-Phase-B state)")
	case now.Before(meta.AccessExpiresAt):
		left := meta.AccessExpiresAt.Sub(now).Round(time.Second)
		fmt.Printf("Auth:    valid (access token expires in %s)\n", left)
	default:
		fmt.Println("Auth:    access token expired — waired-agent will auto-refresh on next request")
	}

	if !meta.DeviceAuthExpiresAt.IsZero() {
		days := int(meta.DeviceAuthExpiresAt.Sub(now).Hours() / 24)
		if days < 0 {
			fmt.Printf("Device key expired %d days ago — run `waired init` to re-OAuth\n", -days)
		} else {
			fmt.Printf("Device key expires in: %d days\n", days)
		}
	}

	if refresh == "" {
		fmt.Println("Refresh: disabled (no refresh token; enrolled before Phase A)")
	} else if meta.NeedsReauth() {
		fmt.Println("Refresh: stopped (terminal auth error; will not retry until `waired init`)")
	} else {
		fmt.Println("Refresh: enabled (auto-refresh runs in waired-agent)")
	}
	return nil
}

// displayDeviceName picks DeviceName when available (post-Phase-3 identity.json),
// falls back to DeviceID for older state files.
func displayDeviceName(id *identity.Identity) string {
	if id.DeviceName != "" {
		return id.DeviceName
	}
	return id.DeviceID
}
