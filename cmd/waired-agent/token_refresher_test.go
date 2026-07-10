package main

import (
	"errors"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
)

func TestTokenRefresherNextSleep(t *testing.T) {
	now := time.Date(2026, 5, 19, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		access  time.Time
		want    time.Duration
		wantMin bool
	}{
		{"zero expiry → minSleep", time.Time{}, 30 * time.Second, true},
		{"expiry far in future → expiry - lead", now.Add(15 * time.Minute), 13 * time.Minute, false},
		{"expiry inside lead → minSleep", now.Add(30 * time.Second), 30 * time.Second, true},
		{"expired → minSleep", now.Add(-1 * time.Hour), 30 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTokenRefresher(tokenRefresherConfig{
				StateDir:       t.TempDir(),
				ControlURL:     "http://cp",
				DeviceID:       "d",
				NetworkID:      "n",
				InitialAccess:  "tok",
				InitialRefresh: "r",
				InitialMeta:    identity.TokenMeta{AccessExpiresAt: tc.access},
			})
			got := r.nextSleep(now)
			if tc.wantMin {
				if got != r.minSleep {
					t.Fatalf("got %v, want minSleep %v", got, r.minSleep)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTokenRefresherCanRefresh(t *testing.T) {
	cases := []struct {
		name string
		cfg  tokenRefresherConfig
		want bool
	}{
		{
			name: "happy",
			cfg: tokenRefresherConfig{
				StateDir:       t.TempDir(),
				ControlURL:     "http://cp",
				DeviceID:       "d",
				NetworkID:      "n",
				MachineKey:     &devicekeys.MachineKey{},
				InitialRefresh: "r",
			},
			want: true,
		},
		{"no refresh", tokenRefresherConfig{StateDir: t.TempDir(), ControlURL: "x", DeviceID: "d", NetworkID: "n", MachineKey: &devicekeys.MachineKey{}}, false},
		{"no machine key", tokenRefresherConfig{StateDir: t.TempDir(), ControlURL: "x", DeviceID: "d", NetworkID: "n", InitialRefresh: "r"}, false},
		{"no control url", tokenRefresherConfig{StateDir: t.TempDir(), DeviceID: "d", NetworkID: "n", MachineKey: &devicekeys.MachineKey{}, InitialRefresh: "r"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTokenRefresher(tc.cfg)
			if got := r.canRefresh(); got != tc.want {
				t.Fatalf("canRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTokenRefresherMarkReauthRequiredPersistsMeta covers the #115
// Phase C flag-persistence path: when the refresh loop classifies a
// terminal auth failure, markReauthRequired must write the on-disk
// flag so subsequent `waired auth status` (and any future tray /
// web-admin surface) can tell the operator what happened.
func TestTokenRefresherMarkReauthRequiredPersistsMeta(t *testing.T) {
	dir := t.TempDir()
	access := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	auth := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	r := newTokenRefresher(tokenRefresherConfig{
		StateDir:       dir,
		ControlURL:     "http://cp",
		DeviceID:       "d",
		NetworkID:      "n",
		MachineKey:     &devicekeys.MachineKey{},
		InitialAccess:  "tok",
		InitialRefresh: "r",
		InitialMeta:    identity.TokenMeta{AccessExpiresAt: access, DeviceAuthExpiresAt: auth},
	})

	r.markReauthRequired(errors.New("cause"))

	got, err := identity.LoadTokenMeta(dir)
	if err != nil {
		t.Fatalf("LoadTokenMeta: %v", err)
	}
	if !got.NeedsReauth() {
		t.Fatalf("expected NeedsReauth==true after markReauthRequired")
	}
	if !got.AccessExpiresAt.Equal(access) || !got.DeviceAuthExpiresAt.Equal(auth) {
		t.Fatalf("markReauthRequired must preserve existing expiries; got %+v", got)
	}
}
