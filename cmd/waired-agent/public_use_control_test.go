package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
)

// Reuses writePublicUse / quietLogger from the existing package-main
// test fixtures (public_grants_test.go, reconcile_test.go).

func TestPublicUseController_ResolvesPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           string
		consentVersion int
		want           router.PublicPolicy
	}{
		{
			name: "no consent forces off but is still nudgeable",
			mode: agentconfig.PublicUseModeAuto, consentVersion: 0,
			want: router.PublicPolicy{Mode: router.PublicModeOff, Consented: false, Main: true, Sub: true},
		},
		{
			name: "stale consent version forces off",
			mode: agentconfig.PublicUseModeExplicit, consentVersion: publicUseWarningVersion - 1,
			want: router.PublicPolicy{Mode: router.PublicModeOff, Consented: false, Main: true, Sub: true},
		},
		{
			name: "auto with current consent",
			mode: agentconfig.PublicUseModeAuto, consentVersion: publicUseWarningVersion,
			want: router.PublicPolicy{Mode: router.PublicModeAuto, Consented: true, Main: true, Sub: true},
		},
		{
			name: "explicit with current consent",
			mode: agentconfig.PublicUseModeExplicit, consentVersion: publicUseWarningVersion,
			want: router.PublicPolicy{Mode: router.PublicModeExplicit, Consented: true, Main: true, Sub: true},
		},
		{
			name: "consented and deliberately off is not nudgeable",
			mode: agentconfig.PublicUseModeOff, consentVersion: publicUseWarningVersion,
			want: router.PublicPolicy{Mode: router.PublicModeOff, Consented: true, Main: true, Sub: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePublicUse(t, dir, tc.mode, tc.consentVersion)
			c := newPublicUseController(path, publicUseWarningVersion, nil, quietLogger())
			if got := c.Policy(); got != tc.want {
				t.Fatalf("Policy() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPublicUseController_FailsClosed(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		c := newPublicUseController(filepath.Join(t.TempDir(), "absent.json"),
			publicUseWarningVersion, nil, quietLogger())
		if got := c.Policy(); got != (router.PublicPolicy{}) {
			t.Fatalf("Policy() = %+v, want zero", got)
		}
	})

	t.Run("unreadable file drops a previously permissive policy", func(t *testing.T) {
		dir := t.TempDir()
		path := writePublicUse(t, dir, agentconfig.PublicUseModeExplicit, publicUseWarningVersion)
		c := newPublicUseController(path, publicUseWarningVersion, nil, quietLogger())
		if c.Policy().Mode != router.PublicModeExplicit {
			t.Fatalf("setup: Policy().Mode = %v", c.Policy().Mode)
		}
		// Corrupt the file the way a partial write would, then invalidate.
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.Reload()
		if got := c.Policy(); got != (router.PublicPolicy{}) {
			t.Fatalf("Policy() = %+v after a parse error, want zero (fail closed)", got)
		}
	})
}

func TestPublicUseController_ReloadPicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, agentconfig.PublicUseModeOff, publicUseWarningVersion)
	c := newPublicUseController(path, publicUseWarningVersion, nil, quietLogger())
	if c.Policy().Mode != router.PublicModeOff {
		t.Fatalf("setup: mode = %v", c.Policy().Mode)
	}
	writePublicUse(t, dir, agentconfig.PublicUseModeAuto, publicUseWarningVersion)
	c.Reload()
	if got := c.Policy().Mode; got != router.PublicModeAuto {
		t.Fatalf("mode after Reload = %v, want auto", got)
	}
}

func TestPublicUseController_NudgeIsOneShotAndAnonymous(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, agentconfig.PublicUseModeOff, 0)
	ring := observability.NewRing(16)
	c := newPublicUseController(path, publicUseWarningVersion, ring, quietLogger())

	for i := 0; i < 5; i++ {
		c.Nudge(router.PublicNudge{ModelID: "qwen3-8b-instruct", Reason: router.NudgeReasonNoCandidate})
	}

	events, _, _ := ring.Since(0, []observability.Kind{observability.KindPublicShareNudge}, 100)
	if len(events) != 1 {
		t.Fatalf("nudge events = %d, want exactly 1", len(events))
	}
	ev := events[0].PublicShareNudge
	if ev == nil {
		t.Fatal("event carries no PublicShareNudge payload")
	}
	if ev.Message != observability.PublicShareNudgeMessage {
		t.Errorf("Message = %q", ev.Message)
	}
	// The hint names no node and asserts no tier: pre-consent there is
	// nothing observable to name (see waired/docs/decisions.md 20260720).
	if ev.Model != "qwen3-8b-instruct" || ev.Reason != router.NudgeReasonNoCandidate {
		t.Errorf("payload = %+v", ev)
	}
}

func TestPublicUseController_NilRingDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := writePublicUse(t, dir, agentconfig.PublicUseModeOff, 0)
	c := newPublicUseController(path, publicUseWarningVersion, nil, quietLogger())
	c.Nudge(router.PublicNudge{ModelID: "m", Reason: router.NudgeReasonNoCandidate})
}

func TestPublicModeOf_UnknownFailsClosed(t *testing.T) {
	if got := publicModeOf("something-new"); got != router.PublicModeOff {
		t.Fatalf("publicModeOf(unknown) = %v, want off", got)
	}
}
