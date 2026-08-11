package main

import (
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/console"
)

// Whether this CLI's output stream can carry non-ASCII glyphs at all.
//
// The OS-specific half — putting a Windows console on CP_UTF8 and reading back
// what it ended up on — lives in internal/platform/console, shared with the
// other two binaries. What is here is the decision that reads it.

// glyphFacts are the environment facts the glyph decision reads. They are
// collected separately from the decision so the decision itself stays a pure
// function that can be table-tested over all three GOOS values (CLAUDE.md
// §Test discipline: put the seam below the behaviour under test).
type glyphFacts struct {
	noEmoji     bool   // WAIRED_NO_EMOJI is set to something non-empty
	locale      string // first non-empty of LC_ALL, LC_CTYPE, LANG
	consoleUTF8 bool   // windows: the console output code page is CP_UTF8
	stdoutTTY   bool   // stdout is attached to a terminal, not a pipe or a file
}

// glyphsSupported reports whether the output stream can carry non-ASCII glyphs
// — emoji, box drawing, em dashes, ellipses.
//
// Windows keys off the console code page rather than the locale variables:
// LC_ALL / LC_CTYPE / LANG are a Unix convention and are normally unset there,
// so before #629 every Windows console took the ASCII path. That is also why
// the reported mojibake was not in emoji (already suppressed) but in the em
// dashes and ellipses the ASCII path itself still emitted — see asciiFold.
func glyphsSupported(goos string, f glyphFacts) bool {
	if f.noEmoji {
		return false
	}
	if goos == "windows" {
		return f.consoleUTF8
	}
	return localeIsUTF8(f.locale)
}

// localeIsUTF8 reports whether a POSIX locale string names a UTF-8 charset.
func localeIsUTF8(locale string) bool {
	u := strings.ToUpper(locale)
	return strings.Contains(u, "UTF-8") || strings.Contains(u, "UTF8")
}

// currentGlyphFacts reads the facts from the live process. Kept apart from
// glyphsSupported so the decision has no environment access of its own.
func currentGlyphFacts() glyphFacts {
	return glyphFacts{
		noEmoji:     os.Getenv("WAIRED_NO_EMOJI") != "",
		locale:      firstSetLocale(),
		consoleUTF8: console.OutputIsUTF8(),
		stdoutTTY:   isTerminal(os.Stdout),
	}
}

// firstSetLocale returns the first of LC_ALL, LC_CTYPE, LANG that is set,
// empty when none are. Precedence matters: LC_ALL=C with LANG=en_US.UTF-8 is a
// non-UTF-8 environment, so the first one set wins outright rather than the
// first one that happens to mention UTF-8.
func firstSetLocale() string {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
