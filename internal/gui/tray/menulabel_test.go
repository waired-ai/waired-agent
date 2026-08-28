package tray

import (
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// escapeMenuLabel is a product contract about what reaches the screen, and it
// is per-renderer in both directions: Win32 eats a lone `&` and Linux and
// macOS draw one; dbusmenu eats a lone `_` and Win32 and macOS draw one; and
// the two Linux renderers eat it differently from each other. Doubling
// everything everywhere would be the same defect with the sign flipped, so
// the table runs every value (waired-agent#1096, waired-agent#1100).

// dialects is every column of the table. The key is what the failure message
// prints, so it names the renderer rather than the enum.
var dialects = map[string]struct {
	goos    string
	dialect trayhost.MenuDialect
}{
	"windows":           {"windows", trayhost.MenuDialectSpec},
	"darwin":            {"darwin", trayhost.MenuDialectSpec},
	"linux/spec":        {"linux", trayhost.MenuDialectSpec},
	"linux/gnome-shell": {"linux", trayhost.MenuDialectGnomeShell},
}

func TestEscapeMenuLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			// The row #1096 was found on. Win32 menus read `&` as the
			// mnemonic prefix, so this drew as "Privacy  safety…" with an
			// underlined s on pc-dell-premium. Both Linux renderers draw
			// an ampersand as text.
			name: "an ampersand survives on Windows only by being doubled",
			in:   "Privacy & safety…",
			want: map[string]string{
				"windows":           "Privacy && safety…",
				"darwin":            "Privacy & safety…",
				"linux/spec":        "Privacy & safety…",
				"linux/gnome-shell": "Privacy & safety…",
			},
		},
		{
			name: "every ampersand is doubled, not just the first",
			in:   "R&D & friends",
			want: map[string]string{
				"windows":           "R&&D && friends",
				"darwin":            "R&D & friends",
				"linux/spec":        "R&D & friends",
				"linux/gnome-shell": "R&D & friends",
			},
		},
		{
			// A peer whose agent predates active_model falls back to the
			// engine's own tag, quantisation suffix and all. Two
			// underscores: the case that has no single right answer.
			name: "an engine tag needs a different escape on each Linux renderer",
			in:   "qwen3.6:35b-a3b-q4_K_M",
			want: map[string]string{
				"windows":           "qwen3.6:35b-a3b-q4_K_M",
				"darwin":            "qwen3.6:35b-a3b-q4_K_M",
				"linux/spec":        "qwen3.6:35b-a3b-q4__K__M",
				"linux/gnome-shell": "qwen3.6:35b-a3b-q4__K_M",
			},
		},
		{
			name: "the Claude row names an environment variable",
			in:   "ANTHROPIC_BASE_URL",
			want: map[string]string{
				"windows":           "ANTHROPIC_BASE_URL",
				"darwin":            "ANTHROPIC_BASE_URL",
				"linux/spec":        "ANTHROPIC__BASE__URL",
				"linux/gnome-shell": "ANTHROPIC__BASE_URL",
			},
		},
		{
			// The router's wire tag, as the Recent activity rows carry it.
			name: "a wire tag in the Recent activity rows",
			in:   "engine_not_ready",
			want: map[string]string{
				"windows":           "engine_not_ready",
				"darwin":            "engine_not_ready",
				"linux/spec":        "engine__not__ready",
				"linux/gnome-shell": "engine__not_ready",
			},
		},
		{
			// One underscore is where the two Linux answers coincide:
			// emails, home directories and device ids are usually this.
			name: "one underscore escapes the same way for both renderers",
			in:   "first_last@corp.com",
			want: map[string]string{
				"windows":           "first_last@corp.com",
				"darwin":            "first_last@corp.com",
				"linux/spec":        "first__last@corp.com",
				"linux/gnome-shell": "first__last@corp.com",
			},
		},
		{
			name: "a device id",
			in:   "dev_28ab996e",
			want: map[string]string{
				"windows":           "dev_28ab996e",
				"darwin":            "dev_28ab996e",
				"linux/spec":        "dev__28ab996e",
				"linux/gnome-shell": "dev__28ab996e",
			},
		},
		{
			// A trailing underscore is the case where doubling is the
			// defect: gnome-shell's regex needs a following character, so
			// it eats nothing and the label is already right.
			name: "a trailing underscore is left alone for gnome-shell",
			in:   "abc_",
			want: map[string]string{
				"windows":           "abc_",
				"darwin":            "abc_",
				"linux/spec":        "abc__",
				"linux/gnome-shell": "abc_",
			},
		},
		{
			// The input is TEXT. A label that already contains two
			// underscores means two underscores on screen, so both
			// renderers need more, not fewer.
			name: "a doubled underscore in the source text is still text",
			in:   "a__b",
			want: map[string]string{
				"windows":           "a__b",
				"darwin":            "a__b",
				"linux/spec":        "a____b",
				"linux/gnome-shell": "a___b",
			},
		},
		{
			name: "a leading underscore",
			in:   "_a",
			want: map[string]string{
				"windows":           "_a",
				"darwin":            "_a",
				"linux/spec":        "__a",
				"linux/gnome-shell": "__a",
			},
		},
		{
			// Both markup characters at once: each OS escapes only its own.
			name: "each renderer escapes only the character it eats",
			in:   "R&D dev_user",
			want: map[string]string{
				"windows":           "R&&D dev_user",
				"darwin":            "R&D dev_user",
				"linux/spec":        "R&D dev__user",
				"linux/gnome-shell": "R&D dev__user",
			},
		},
		{
			name: "an already-doubled ampersand is doubled again, because the input is text",
			in:   "A && B",
			want: map[string]string{
				"windows":           "A &&&& B",
				"darwin":            "A && B",
				"linux/spec":        "A && B",
				"linux/gnome-shell": "A && B",
			},
		},
		{
			name: "a label with nothing special is untouched",
			in:   "● Peers: 2 of 4 serving",
			want: map[string]string{
				"windows":           "● Peers: 2 of 4 serving",
				"darwin":            "● Peers: 2 of 4 serving",
				"linux/spec":        "● Peers: 2 of 4 serving",
				"linux/gnome-shell": "● Peers: 2 of 4 serving",
			},
		},
		{
			name: "empty stays empty",
			in:   "",
			want: map[string]string{
				"windows": "", "darwin": "", "linux/spec": "", "linux/gnome-shell": "",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, col := range dialects {
				got := escapeMenuLabel(col.goos, col.dialect, tc.in)
				if got != tc.want[name] {
					t.Errorf("%s: escapeMenuLabel(%q) = %q, want %q", name, tc.in, got, tc.want[name])
				}
			}
		})
	}
}

// The two Linux renderers, transcribed from their own source, so the escape
// can be checked against what it is compensating for rather than against a
// second copy of my arithmetic.

// gnomeShellRender is gnome-shell-extension-appindicator's dbusMenu.js:
//
//	propertyGet('label').replace(/_([^_])/, '$1')
//
// A non-global regex, so exactly one match: the first underscore that is
// followed by a non-underscore character disappears, and nothing else
// changes. Verified verbatim against the installed extension on the fleet's
// GNOME host (Shell 50.1, ubuntu-appindicators).
func gnomeShellRender(s string) string {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '_' && s[i+1] != '_' {
			return s[:i] + s[i+1:]
		}
	}
	return s
}

// specRender is the dbusmenu specification's rule, which is what Plasma's
// vendored swapMnemonicChar and libdbusmenu-gtk implement: `__` draws one
// underscore, and every remaining single underscore is not displayed at all
// (the first of them marking the access key, which we never set).
func specRender(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '_' {
			b.WriteByte('_')
			i++
		}
	}
	return b.String()
}

// TestEscapeMenuLabel_SurvivesItsRenderer is the assertion that matters: for
// every shape of label the tray actually shows, running the escape and then
// the renderer it was written for gives back the text we meant. This is what
// makes the table above a fix rather than a rearrangement — and it fails if
// either escape is changed without the renderer model changing with it.
func TestEscapeMenuLabel_SurvivesItsRenderer(t *testing.T) {
	labels := []string{
		"qwen3.6:35b-a3b-q4_K_M",
		"ANTHROPIC_BASE_URL",
		"engine_not_ready",
		"qwen3:8b — dev_28ab996e → dev_301ad2a1 (share_off, 2m ago)",
		"first_last@corp.com",
		"/home/dev_user/.config/opencode/plugin/waired.js",
		"⚠ vLLM could not bind: set inference.vllm_port in agent.json",
		"abc_",
		"a__b",
		"_a",
		"_",
		"__",
		"● Peers: 2 of 4 serving",
		"Privacy & safety…",
		"",
	}
	for _, in := range labels {
		if got := gnomeShellRender(escapeMenuLabel("linux", trayhost.MenuDialectGnomeShell, in)); got != in {
			t.Errorf("gnome-shell drew %q, want %q", got, in)
		}
		if got := specRender(escapeMenuLabel("linux", trayhost.MenuDialectSpec, in)); got != in {
			t.Errorf("a spec renderer drew %q, want %q", got, in)
		}
	}
}

// TestEscapeMenuLabel_TodaysDefectIsReal keeps the premise honest: without the
// escape, these labels really do lose a character. If a renderer model here
// ever stops eating anything, the fix above has become a no-op and the table
// is measuring nothing (the shape #178 shipped through).
func TestEscapeMenuLabel_TodaysDefectIsReal(t *testing.T) {
	for _, in := range []string{"qwen3.6:35b-a3b-q4_K_M", "engine_not_ready", "first_last@corp.com"} {
		if gnomeShellRender(in) == in {
			t.Errorf("gnome-shell left %q alone — the premise of #1100 no longer holds", in)
		}
		if specRender(in) == in {
			t.Errorf("a spec renderer left %q alone — the premise of #1100 no longer holds", in)
		}
	}
}

// TestEscapeMenuLabel_UnknownGOOSLeavesTextAlone: the escape adds markup, so
// the safe answer for an OS this table has never been checked against is to
// add none. A stray `&&` on screen is a defect; a stray `&` is what the
// string said.
func TestEscapeMenuLabel_UnknownGOOSLeavesTextAlone(t *testing.T) {
	for _, goos := range []string{"freebsd", "openbsd", ""} {
		for _, d := range []trayhost.MenuDialect{trayhost.MenuDialectSpec, trayhost.MenuDialectGnomeShell} {
			if got := escapeMenuLabel(goos, d, "A & B_C"); got != "A & B_C" {
				t.Errorf("escapeMenuLabel(%q, %v, ...) = %q, want the text unchanged", goos, d, got)
			}
		}
	}
}

// TestEscapeMenuLabel_DialectIsReadOnlyOnLinux: Windows and macOS have their
// own markup rules and no underscore rule, so a dialect resolved on some
// other machine — or a zero value — must not change what they draw.
func TestEscapeMenuLabel_DialectIsReadOnlyOnLinux(t *testing.T) {
	for _, goos := range []string{"windows", "darwin"} {
		spec := escapeMenuLabel(goos, trayhost.MenuDialectSpec, "R&D dev_user")
		gnome := escapeMenuLabel(goos, trayhost.MenuDialectGnomeShell, "R&D dev_user")
		if spec != gnome {
			t.Errorf("on %s the dialect changed the label: %q vs %q", goos, spec, gnome)
		}
	}
}

// TestEscapeMenuLabel_MatchesThisHost is the one assertion that runs with the
// real runtime.GOOS, so the Windows and macOS unit-test legs each check their
// own row of the table rather than only Linux's.
func TestEscapeMenuLabel_MatchesThisHost(t *testing.T) {
	got := escapeMenuLabel(runtime.GOOS, trayhost.MenuDialectSpec, "Privacy & safety_1")
	want := "Privacy & safety_1"
	switch runtime.GOOS {
	case "windows":
		want = "Privacy && safety_1"
	case "linux":
		want = "Privacy & safety__1"
	}
	if got != want {
		t.Errorf("on %s: escapeMenuLabel = %q, want %q", runtime.GOOS, got, want)
	}
}
