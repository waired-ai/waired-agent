package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
)

// signInFinding reports whether this device's sign-in still works.
//
// Reads the persisted token metadata rather than asking the daemon, so
// it answers even when the daemon is down — and because that file is
// exactly what the refresh loop writes when it gives up
// (waired-agent#318). Before this, a device whose auto-refresh had
// died showed nothing in `waired doctor`: every check passed while the
// agent 401'd the Control Plane on every push.
func signInFinding(stateDir string) integration.AuditFinding {
	id, err := identity.Load(stateDir)
	if err != nil {
		// A state dir this run may not read is a check that did not
		// happen, and #651 is about saying so instead of dropping the
		// row. Any other error still stays quiet: the gateway-token and
		// live-probe findings cover it with better advice.
		if f, ok := unreadableFinding("device sign-in", err); ok {
			return f
		}
		return integration.AuditFinding{}
	}
	if id == nil {
		// Not enrolled. Covered elsewhere, with better advice.
		return integration.AuditFinding{}
	}
	meta, err := identity.LoadTokenMeta(stateDir)
	if err != nil {
		if f, ok := unreadableFinding("device sign-in", err); ok {
			return f
		}
		return integration.AuditFinding{}
	}
	return signInFindingFrom(meta, time.Now())
}

// signInFindingFrom is the pure decision behind signInFinding.
func signInFindingFrom(meta identity.TokenMeta, now time.Time) integration.AuditFinding {
	if meta.NeedsReauth() {
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "device sign-in",
			Detail:  "no longer valid. Run `waired init` to sign in again on this computer",
		}
	}
	if !meta.DeviceAuthExpiresAt.IsZero() && now.After(meta.DeviceAuthExpiresAt) {
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "device sign-in",
			Detail:  "expired. Run `waired init` to sign in again on this computer",
		}
	}
	return integration.AuditFinding{
		Status:  integration.StatusOK,
		Subject: "device sign-in",
		Detail:  "valid",
	}
}

// connectionFinding reports whether a signed-in device actually brought
// its network connection up.
//
// Separate from sign-in on purpose: a device can be perfectly signed in
// and still fail to connect — the case that motivated this was a
// WireGuard port that had become unbindable between boots. That used to
// present as "not signed in" everywhere, which sent people to `waired
// init` for a problem re-running it would not fix.
//
// The read goes through mgmtReadRoute for the reason daemonIdentity states:
// /waired/v1/identity is socket-only, so over plain TCP this returned 403
// and the row vanished from every doctor run (#785).
func connectionFinding(ctx context.Context, mgmt string) integration.AuditFinding {
	target, cl, err := mgmtReadRoute(mgmtURL(mgmt, identityPath), 3*time.Second)
	if err != nil {
		return integration.AuditFinding{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return integration.AuditFinding{}
	}
	resp, err := cl.Do(req)
	if err != nil {
		// The daemon is unreachable; the management probe reports that
		// with a better message than we could here.
		return integration.AuditFinding{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return integration.AuditFinding{}
	}
	var v management.IdentityView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return integration.AuditFinding{}
	}
	return connectionFindingFrom(v)
}

// connectionFindingFrom is the pure decision behind connectionFinding.
func connectionFindingFrom(v management.IdentityView) integration.AuditFinding {
	if !v.Enrolled {
		// Not signed in: nothing to say about a connection.
		return integration.AuditFinding{}
	}
	if v.AuthState == management.AuthStateReauthRequired {
		// signInFinding already tells this story from disk; saying it
		// twice under a second subject reads like two problems.
		return integration.AuditFinding{}
	}
	if v.Active {
		return integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "network connection",
			Detail:  "connected",
		}
	}
	detail := "signed in, but this computer isn't connected yet. Waired keeps retrying"
	if v.ActivationError != "" {
		detail = fmt.Sprintf("signed in, but this computer couldn't connect: %s. Waired keeps retrying", v.ActivationError)
	}
	return integration.AuditFinding{
		Status:  integration.StatusWarn,
		Subject: "network connection",
		Detail:  detail,
	}
}
