//go:build linux

package trayhost

import "testing"

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name       string
		in         facts
		wantStatus Status
		wantHint   bool // whether Hint should be non-empty
	}{
		{
			name:       "no display is not applicable",
			in:         facts{hasDisplay: false, desktop: DesktopGNOME},
			wantStatus: NotApplicable,
		},
		{
			name:       "host registered renders regardless of desktop",
			in:         facts{hasDisplay: true, hostRegistered: true, desktop: DesktopGNOME},
			wantStatus: HostPresent,
		},
		{
			name:       "GNOME without host warns with hint",
			in:         facts{hasDisplay: true, hostRegistered: false, desktop: DesktopGNOME, wayland: true},
			wantStatus: NoHost,
			wantHint:   true,
		},
		{
			name:       "MATE without host is unsupported with hint",
			in:         facts{hasDisplay: true, hostRegistered: false, desktop: DesktopMATE},
			wantStatus: Unsupported,
			wantHint:   true,
		},
		{
			name:       "KDE without host falls back to generic NoHost",
			in:         facts{hasDisplay: true, hostRegistered: false, desktop: DesktopKDE},
			wantStatus: NoHost,
			wantHint:   true,
		},
		{
			name:       "other desktop without host is generic NoHost",
			in:         facts{hasDisplay: true, hostRegistered: false, desktop: DesktopOther},
			wantStatus: NoHost,
			wantHint:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluate(tc.in)
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v", got.Status, tc.wantStatus)
			}
			if (got.Hint != "") != tc.wantHint {
				t.Errorf("Hint present = %v (%q), want %v", got.Hint != "", got.Hint, tc.wantHint)
			}
			// Desktop/Wayland always pass through untouched.
			if got.Desktop != tc.in.desktop {
				t.Errorf("Desktop = %v, want %v", got.Desktop, tc.in.desktop)
			}
			if got.Wayland != tc.in.wayland {
				t.Errorf("Wayland = %v, want %v", got.Wayland, tc.in.wayland)
			}
		})
	}
}

func TestParseDesktop(t *testing.T) {
	tests := []struct {
		in   string
		want Desktop
	}{
		{"", DesktopUnknown},
		{"GNOME", DesktopGNOME},
		{"ubuntu:GNOME", DesktopGNOME},
		{"gnome", DesktopGNOME},
		{"KDE", DesktopKDE},
		{"plasma:KDE", DesktopKDE},
		{"MATE", DesktopMATE},
		{"X-Cinnamon", DesktopOther},
		{"sway", DesktopOther},
	}
	for _, tc := range tests {
		if got := parseDesktop(tc.in); got != tc.want {
			t.Errorf("parseDesktop(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestMenuDialectFor pins the one renderer we write differently for, and the
// direction the unknown case falls in. Getting that direction wrong is the
// expensive mistake: spec markup on a spec renderer is correct, while
// gnome-shell markup anywhere else puts a stray underscore on the screen.
func TestMenuDialectFor(t *testing.T) {
	for _, tc := range []struct {
		comm string
		want MenuDialect
	}{
		{"gnome-shell", MenuDialectGnomeShell},
		// Plasma, XFCE and Waybar all render through a spec-compliant
		// importer (plasma-workspace's vendored swapMnemonicChar, or
		// libdbusmenu-gtk), and snixembed proxies through GTK.
		{"plasmashell", MenuDialectSpec},
		{"xfce4-panel", MenuDialectSpec},
		{"waybar", MenuDialectSpec},
		{"snixembed", MenuDialectSpec},
		// No session bus, no watcher on it, or an unreadable /proc entry.
		{"", MenuDialectSpec},
		// Near-misses must not match: the case is an equality, not a
		// substring, so a different shell does not inherit GNOME's quirk.
		{"gnome-shell-calendar-server", MenuDialectSpec},
		{"Gnome-Shell", MenuDialectSpec},
	} {
		if got := menuDialectFor(tc.comm); got != tc.want {
			t.Errorf("menuDialectFor(%q) = %v, want %v", tc.comm, got, tc.want)
		}
	}
}

// TestMenuLabelsDoesNotPanicWithoutASession: the probe runs at tray startup on
// every Linux host, including one with no session bus at all (a headless
// server running `waired doctor`). It must answer, not crash.
func TestMenuLabelsDoesNotPanicWithoutASession(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/waired-test-bus")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if got := MenuLabels(); got != MenuDialectSpec {
		t.Errorf("MenuLabels() with no bus = %v, want %v", got, MenuDialectSpec)
	}
}
