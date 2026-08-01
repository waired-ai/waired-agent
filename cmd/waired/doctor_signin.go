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
	if err != nil || id == nil {
		// Not enrolled, or unreadable state. The gateway-token and
		// live-probe findings already cover both, with better advice.
		return integration.AuditFinding{}
	}
	meta, err := identity.LoadTokenMeta(stateDir)
	if err != nil {
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
			Detail:  "no longer valid — run `waired init` to sign in again on this device",
		}
	}
	if !meta.DeviceAuthExpiresAt.IsZero() && now.After(meta.DeviceAuthExpiresAt) {
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "device sign-in",
			Detail:  "expired — run `waired init` to sign in again on this device",
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
func connectionFinding(ctx context.Context, mgmtURL string) integration.AuditFinding {
	cl := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mgmtURL+"/waired/v1/identity", nil)
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
	detail := "signed in, but this device is not connected yet — Waired keeps retrying"
	if v.ActivationError != "" {
		detail = fmt.Sprintf("signed in, but this device could not connect: %s — Waired keeps retrying", v.ActivationError)
	}
	return integration.AuditFinding{
		Status:  integration.StatusWarn,
		Subject: "network connection",
		Detail:  detail,
	}
}
