package tray

import (
	"runtime"
	"testing"
)

// escapeMenuLabel is a product contract about what reaches the screen, and
// it is per-OS in both directions: Windows eats a lone `&`, and Linux and
// macOS draw one. Doubling it everywhere would be the same defect with the
// sign flipped, so the table runs all three (waired-agent#1096).

func TestEscapeMenuLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string // goos -> want
	}{
		{
			// The row the defect was found on. Win32 menus read `&` as
			// the mnemonic prefix, so this drew as "Privacy  safety…"
			// with an underlined s on pc-dell-premium.
			name: "an ampersand survives on Windows only by being doubled",
			in:   "Privacy & safety…",
			want: map[string]string{
				"windows": "Privacy && safety…",
				"darwin":  "Privacy & safety…",
				"linux":   "Privacy & safety…",
			},
		},
		{
			name: "every ampersand is doubled, not just the first",
			in:   "R&D & friends",
			want: map[string]string{
				"windows": "R&&D && friends",
				"darwin":  "R&D & friends",
				"linux":   "R&D & friends",
			},
		},
		{
			// Underscores are deliberately left alone — see the file
			// comment. This pins the decision so a later "obvious"
			// symmetry does not silently reintroduce it.
			name: "underscores are not escaped on any OS",
			in:   "qwen3.6:35b-a3b-q4_K_M",
			want: map[string]string{
				"windows": "qwen3.6:35b-a3b-q4_K_M",
				"darwin":  "qwen3.6:35b-a3b-q4_K_M",
				"linux":   "qwen3.6:35b-a3b-q4_K_M",
			},
		},
		{
			name: "an already-doubled ampersand is doubled again, because the input is text",
			in:   "A && B",
			want: map[string]string{
				"windows": "A &&&& B",
				"darwin":  "A && B",
				"linux":   "A && B",
			},
		},
		{
			name: "a label with nothing special is untouched",
			in:   "● Peers: 2 of 4 serving",
			want: map[string]string{
				"windows": "● Peers: 2 of 4 serving",
				"darwin":  "● Peers: 2 of 4 serving",
				"linux":   "● Peers: 2 of 4 serving",
			},
		},
		{
			name: "empty stays empty",
			in:   "",
			want: map[string]string{"windows": "", "darwin": "", "linux": ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, goos := range []string{"windows", "darwin", "linux"} {
				if got := escapeMenuLabel(goos, tc.in); got != tc.want[goos] {
					t.Errorf("escapeMenuLabel(%q, %q) = %q, want %q", goos, tc.in, got, tc.want[goos])
				}
			}
		})
	}
}

// TestEscapeMenuLabel_UnknownGOOSLeavesTextAlone: the escape adds markup,
// so the safe answer for an OS this table has never been checked against
// is to add none. A stray `&&` on screen is a defect; a stray `&` is what
// the string said.
func TestEscapeMenuLabel_UnknownGOOSLeavesTextAlone(t *testing.T) {
	for _, goos := range []string{"freebsd", "openbsd", ""} {
		if got := escapeMenuLabel(goos, "A & B"); got != "A & B" {
			t.Errorf("escapeMenuLabel(%q, ...) = %q, want the text unchanged", goos, got)
		}
	}
}

// TestEscapeMenuLabel_MatchesThisHost is the one assertion that runs with
// the real runtime.GOOS, so the Windows and macOS unit-test legs each
// check their own row of the table rather than only Linux's.
func TestEscapeMenuLabel_MatchesThisHost(t *testing.T) {
	got := escapeMenuLabel(runtime.GOOS, "Privacy & safety…")
	want := "Privacy & safety…"
	if runtime.GOOS == "windows" {
		want = "Privacy && safety…"
	}
	if got != want {
		t.Errorf("on %s: escapeMenuLabel = %q, want %q", runtime.GOOS, got, want)
	}
}
