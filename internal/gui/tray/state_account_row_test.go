package tray

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// The signed-in account row. It used to be greyed out beside the header,
// which reads as "this account is inactive" — the same misreading the
// status rows had. It is now a live row that opens that account's page in
// the console (owner request, 2026-08-28).

func TestAccountURL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controlURL string
		want       string
	}{
		{
			// The console is a single-page app served under /admin (its
			// vite base) with `account` as one of its routes.
			name:       "the account page is a route under the console",
			controlURL: "https://control.example.com",
			want:       "https://control.example.com/admin/account",
		},
		{
			name:       "a trailing slash does not double up",
			controlURL: "https://control.example.com/",
			want:       "https://control.example.com/admin/account",
		},
		{
			// No control URL means no console to send anyone to. The row
			// still renders — it is naming who you are signed in as — and
			// the click is simply inert.
			name:       "no control URL yields no link",
			controlURL: "",
			want:       "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountURL(tc.controlURL); got != tc.want {
				t.Errorf("accountURL(%q) = %q, want %q", tc.controlURL, got, tc.want)
			}
		})
	}
}

func TestUpdate_Connected_CarriesTheAccountURL(t *testing.T) {
	id := &management.IdentityView{
		Enrolled: true, AccountEmail: "alice@example.com",
		ControlURL: "https://control.example.com",
	}
	got := Update(Snapshot{Health: HealthOnline, Identity: id, Status: &management.Status{Phase: "active"}})
	if got.AccountURL != "https://control.example.com/admin/account" {
		t.Errorf("AccountURL=%q", got.AccountURL)
	}
}

// TestUpdate_DaemonDown_HasNoAccountURL: the daemon-down menu renders the
// LAST email it saw, not a live identity. A row naming an account this
// poll could not confirm has nowhere honest to send you, so the click is
// inert there rather than opening a page about an account the tray is
// only remembering.
func TestUpdate_DaemonDown_HasNoAccountURL(t *testing.T) {
	got := offlineModel(MenuModel{AccountEmail: "alice@example.com"}, false, daemonDownFacts{
		ServiceInstalled: true, LastEmail: "alice@example.com",
	})
	if got.AccountEmail == "" {
		t.Fatalf("the daemon-down menu dropped the last known account")
	}
	if got.AccountURL != "" {
		t.Errorf("AccountURL=%q on the daemon-down menu, want empty", got.AccountURL)
	}
}

func TestOnAccount_OpensTheConsoleAccountPage(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{}
	tr.last = MenuModel{AccountURL: "https://control.example.com/admin/account"}

	tr.onAccount()

	got := l.snapshot(&l.browsers)
	if len(got) != 1 || got[0] != "https://control.example.com/admin/account" {
		t.Fatalf("browsers = %v, want the account page once", got)
	}
}

func TestOnAccount_SilentWithNoURL(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{}
	tr.last = MenuModel{AccountEmail: "alice@example.com"}

	tr.onAccount()

	if got := l.snapshot(&l.browsers); len(got) != 0 {
		t.Errorf("browsers = %v, want none", got)
	}
	if got := l.snapshot(&l.errors); len(got) != 0 {
		t.Errorf("errors = %v — an account row with no console is not an error", got)
	}
}
