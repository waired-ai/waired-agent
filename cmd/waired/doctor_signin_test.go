package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
)

// TestSignInFindingFrom pins the product contract from
// waired-agent#318: when auto-refresh has given up, `waired doctor` must
// say so and name the recovery command. The live incident ran for hours
// with every doctor check green while the agent 401'd every push.
func TestSignInFindingFrom(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		meta       identity.TokenMeta
		wantStatus integration.Status
		wantDetail string // substring
	}{
		{
			name:       "healthy",
			meta:       identity.TokenMeta{DeviceAuthExpiresAt: now.Add(90 * 24 * time.Hour)},
			wantStatus: integration.StatusOK,
		},
		{
			// The reuse-detected lockout: months of login validity left,
			// but refresh will never succeed again.
			name: "reauth required outranks a valid-looking window",
			meta: identity.TokenMeta{
				ReauthRequiredAt:    now.Add(-time.Hour),
				DeviceAuthExpiresAt: now.Add(180 * 24 * time.Hour),
			},
			wantStatus: integration.StatusFail,
			wantDetail: "waired init",
		},
		{
			name:       "login window lapsed",
			meta:       identity.TokenMeta{DeviceAuthExpiresAt: now.Add(-24 * time.Hour)},
			wantStatus: integration.StatusFail,
			wantDetail: "waired init",
		},
		{
			// Pre-Phase-B state files carry no expiry. Absence of
			// evidence is not a failure.
			name:       "no expiry recorded",
			meta:       identity.TokenMeta{},
			wantStatus: integration.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := signInFindingFrom(tc.meta, now)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v (detail %q)", got.Status, tc.wantStatus, got.Detail)
			}
			if got.Subject == "" {
				t.Fatal("finding must carry a subject or the caller drops it")
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("detail %q does not mention %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// TestSignInFindingSkipsUnenrolled pins that a machine with no identity
// produces no finding: there is nothing to re-authenticate, and the
// enrollment story is told by other checks.
func TestSignInFindingSkipsUnenrolled(t *testing.T) {
	if got := signInFinding(t.TempDir()); got.Subject != "" {
		t.Fatalf("expected no finding for an unenrolled state dir, got %+v", got)
	}
}

// TestConnectionFindingFrom pins the distinction waired-agent#318 turned
// on: "signed in but not connected" must not read as "signed out". The
// third symptom — a WireGuard port that had become unbindable — showed
// up to users as being logged out, which sent them to a command that
// would not have fixed it.
func TestConnectionFindingFrom(t *testing.T) {
	cases := []struct {
		name       string
		view       management.IdentityView
		wantStatus integration.Status
		wantSubj   bool
		wantDetail string // substring
	}{
		{
			name:       "connected",
			view:       management.IdentityView{Enrolled: true, Active: true},
			wantStatus: integration.StatusOK,
			wantSubj:   true,
		},
		{
			name: "signed in, activation failed",
			view: management.IdentityView{
				Enrolled:        true,
				Active:          false,
				ActivationError: "device up: bind: access permissions",
			},
			wantStatus: integration.StatusWarn,
			wantSubj:   true,
			wantDetail: "signed in",
		},
		{
			name:       "signed in, activation still in flight",
			view:       management.IdentityView{Enrolled: true},
			wantStatus: integration.StatusWarn,
			wantSubj:   true,
			wantDetail: "not connected yet",
		},
		{
			name:     "not enrolled produces nothing",
			view:     management.IdentityView{},
			wantSubj: false,
		},
		{
			// signInFinding already reports this from disk; two findings
			// for one problem reads like two problems.
			name: "reauth_required is left to the sign-in finding",
			view: management.IdentityView{
				Enrolled:  true,
				AuthState: management.AuthStateReauthRequired,
			},
			wantSubj: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := connectionFindingFrom(tc.view)
			if !tc.wantSubj {
				if got.Subject != "" {
					t.Fatalf("expected no finding, got %+v", got)
				}
				return
			}
			if got.Subject == "" {
				t.Fatal("expected a finding")
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v", got.Status, tc.wantStatus)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("detail %q does not mention %q", got.Detail, tc.wantDetail)
			}
		})
	}
}
