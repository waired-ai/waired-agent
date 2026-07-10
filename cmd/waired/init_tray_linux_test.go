//go:build linux

package main

import "testing"

func TestDecideTrayExtAction(t *testing.T) {
	tests := []struct {
		name string
		in   trayExtFacts
		want trayExtAction
	}{
		{
			name: "GNOME with tray and apt, extension missing -> install",
			in:   trayExtFacts{trayBinaryPresent: true, gnomeShellPresent: true, extensionInstalled: false, aptPresent: true},
			want: trayExtInstall,
		},
		{
			name: "GNOME with tray, extension missing, no apt -> manual hint",
			in:   trayExtFacts{trayBinaryPresent: true, gnomeShellPresent: true, extensionInstalled: false, aptPresent: false},
			want: trayExtManualHint,
		},
		{
			name: "extension already installed -> skip (default Ubuntu Desktop)",
			in:   trayExtFacts{trayBinaryPresent: true, gnomeShellPresent: true, extensionInstalled: true, aptPresent: true},
			want: trayExtSkip,
		},
		{
			name: "not GNOME (e.g. KDE/headless) -> skip",
			in:   trayExtFacts{trayBinaryPresent: true, gnomeShellPresent: false, extensionInstalled: false, aptPresent: true},
			want: trayExtSkip,
		},
		{
			name: "no waired-tray installed -> skip",
			in:   trayExtFacts{trayBinaryPresent: false, gnomeShellPresent: true, extensionInstalled: false, aptPresent: true},
			want: trayExtSkip,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideTrayExtAction(tc.in); got != tc.want {
				t.Errorf("decideTrayExtAction(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
