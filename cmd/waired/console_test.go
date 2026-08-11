package main

import "testing"

// TestGlyphsSupported table-tests the glyph decision over all three GOOS
// values. Record of today's behaviour on Linux/macOS (the locale rules are
// unchanged by #629); product contract on Windows — waired-ai/waired-agent#629
// asks for output that survives a non-UTF-8 console, and the owner ratified
// "set the console to CP_UTF8 and then render the same glyphs the other two
// OSes do" on waired-ai/waired#1127 L37.
func TestGlyphsSupported(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		facts glyphFacts
		want  bool
	}{
		// --- the knob wins everywhere -------------------------------------
		{"linux, WAIRED_NO_EMOJI beats a UTF-8 locale", "linux",
			glyphFacts{noEmoji: true, locale: "en_US.UTF-8"}, false},
		{"darwin, WAIRED_NO_EMOJI beats a UTF-8 locale", "darwin",
			glyphFacts{noEmoji: true, locale: "en_US.UTF-8"}, false},
		{"windows, WAIRED_NO_EMOJI beats a UTF-8 console", "windows",
			glyphFacts{noEmoji: true, consoleUTF8: true}, false},

		// --- unix reads the locale ----------------------------------------
		{"linux UTF-8 locale", "linux", glyphFacts{locale: "en_US.UTF-8"}, true},
		{"linux utf8 spelling", "linux", glyphFacts{locale: "ja_JP.utf8"}, true},
		{"linux C locale", "linux", glyphFacts{locale: "C"}, false},
		{"linux no locale set", "linux", glyphFacts{}, false},
		{"darwin UTF-8 locale", "darwin", glyphFacts{locale: "ja_JP.UTF-8"}, true},
		{"darwin POSIX locale", "darwin", glyphFacts{locale: "POSIX"}, false},

		// A UTF-8 console does not make a C-locale Unix terminal UTF-8:
		// consoleUTF8 is only ever true on Windows, and the unix arm must
		// ignore it even if it somehow were.
		{"linux ignores the console code page", "linux",
			glyphFacts{locale: "C", consoleUTF8: true}, false},

		// --- windows reads the console code page --------------------------
		// This is the #629 arm. Before it existed, windows fell through to
		// the locale variables, which Windows does not set — so every Windows
		// console took the ASCII path, and the mojibake was in the em dashes
		// that path still emitted.
		{"windows CP_UTF8 console", "windows", glyphFacts{consoleUTF8: true}, true},
		{"windows CP932 console", "windows", glyphFacts{consoleUTF8: false}, false},
		{"windows no console (service, pipe)", "windows", glyphFacts{}, false},
		{"windows ignores an inherited unix locale", "windows",
			glyphFacts{locale: "en_US.UTF-8", consoleUTF8: false}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := glyphsSupported(tt.goos, tt.facts); got != tt.want {
				t.Errorf("glyphsSupported(%q, %+v) = %v, want %v",
					tt.goos, tt.facts, got, tt.want)
			}
		})
	}
}

// TestFirstSetLocalePrecedence pins that the first variable that is *set* wins
// outright, rather than the first one that mentions UTF-8: LC_ALL=C with
// LANG=en_US.UTF-8 is a non-UTF-8 environment. Record of today's behaviour,
// carried over from the original localeIsUTF8.
func TestFirstSetLocalePrecedence(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if got := firstSetLocale(); got != "C" {
		t.Errorf("firstSetLocale() = %q, want %q — LC_ALL outranks LANG", got, "C")
	}
	if localeIsUTF8(firstSetLocale()) {
		t.Error("LC_ALL=C read as UTF-8")
	}

	t.Setenv("LC_ALL", "")
	if got := firstSetLocale(); got != "en_US.UTF-8" {
		t.Errorf("firstSetLocale() = %q, want LANG once LC_ALL is unset", got)
	}
}

func TestLocaleIsUTF8(t *testing.T) {
	utf8 := []string{"en_US.UTF-8", "ja_JP.utf8", "C.UTF-8", "de_DE.UTF8"}
	other := []string{"", "C", "POSIX", "ja_JP.eucJP", "en_US.ISO-8859-1"}
	for _, s := range utf8 {
		if !localeIsUTF8(s) {
			t.Errorf("localeIsUTF8(%q) = false, want true", s)
		}
	}
	for _, s := range other {
		if localeIsUTF8(s) {
			t.Errorf("localeIsUTF8(%q) = true, want false", s)
		}
	}
}
