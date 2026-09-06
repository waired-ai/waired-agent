package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// TestReauthWanted is the #313 decision table, modelled on `tailscale
// up`: a run against a device whose credentials still work does not
// re-authenticate, and an auth key does not change that (tailscale#19501
// — if valid state exists, reuse it). Re-auth happens when the operator
// asks for it, or when the credentials are the thing that is broken.
func TestReauthWanted(t *testing.T) {
	cases := []struct {
		name  string
		force bool
		view  *management.IdentityView
		want  bool
	}{
		{"no daemon answer, no flag", false, nil, false},
		{"unenrolled", false, &management.IdentityView{}, false},
		{"enrolled and healthy", false,
			&management.IdentityView{Enrolled: true, AuthState: management.AuthStateOK}, false},
		{"enrolled, older daemon reports no auth state", false,
			&management.IdentityView{Enrolled: true}, false},
		{"enrolled but re-auth required", false,
			&management.IdentityView{Enrolled: true, AuthState: management.AuthStateReauthRequired}, true},
		{"--force-reauth on a healthy device", true,
			&management.IdentityView{Enrolled: true, AuthState: management.AuthStateOK}, true},
		{"--force-reauth with no daemon answer", true, nil, true},
		// reauth_required on a device that is not enrolled cannot happen;
		// pinned so the predicate reads as "enrolled AND broken".
		{"not enrolled but re-auth flagged", false,
			&management.IdentityView{AuthState: management.AuthStateReauthRequired}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reauthWanted(tc.force, tc.view); got != tc.want {
				t.Errorf("reauthWanted(%v, %+v) = %v, want %v", tc.force, tc.view, got, tc.want)
			}
		})
	}
}

// TestResumeLines pins the copy of the resume notice, including the
// half that keeps Waired out of tailscale#7995 ("silently ignores
// authkey argument when already authed"): an auth key that was not used
// is said out loud, with the flag that would have used it.
func TestResumeLines(t *testing.T) {
	cases := []struct {
		name         string
		email        string
		authKeyGiven bool
		want         []string
		absent       []string
	}{
		{
			name:  "known account",
			email: "you@example.com",
			want:  []string{"Already signed in as you@example.com. Resuming setup."},
			// Nothing about a key that was never passed.
			absent: []string{"auth key"},
		},
		{
			name:   "account unknown",
			want:   []string{"This computer is already signed in. Resuming setup."},
			absent: []string{"auth key", " as "},
		},
		{
			name:         "auth key not used",
			email:        "you@example.com",
			authKeyGiven: true,
			want: []string{
				"Already signed in as you@example.com. Resuming setup.",
				"The auth key wasn't used. To sign in again with it, pass --force-reauth.",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(resumeLines(tc.email, tc.authKeyGiven), "\n")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("resume notice missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("resume notice should not mention %q:\n%s", absent, got)
				}
			}
		})
	}
}

// The daemon is the authority on enrollment: the CLI's own state dir can
// be the wrong one (Windows, #313) or unreadable (a standard user
// against the ACL'd ProgramData tree, waired#751). identityFromView is
// how a daemon-reported enrollment reaches the renew summary that used
// to require a readable identity.json.
func TestIdentityFromView(t *testing.T) {
	if got := identityFromView(nil); got != nil {
		t.Errorf("identityFromView(nil) = %+v, want nil", got)
	}
	if got := identityFromView(&management.IdentityView{}); got != nil {
		t.Errorf("identityFromView(unenrolled) = %+v, want nil", got)
	}
	v := &management.IdentityView{
		Enrolled: true, AccountEmail: "you@example.com", DeviceName: "dev-1",
		DeviceID: "dev_abc", NetworkName: "personal", ControlURL: "https://cp.example",
	}
	got := identityFromView(v)
	if got == nil {
		t.Fatal("identityFromView dropped an enrolled device")
	}
	if got.AccountEmail != v.AccountEmail || got.DeviceName != v.DeviceName ||
		got.DeviceID != v.DeviceID || got.NetworkName != v.NetworkName ||
		got.ControlURL != v.ControlURL {
		t.Errorf("identityFromView lost fields: %+v", got)
	}
}
