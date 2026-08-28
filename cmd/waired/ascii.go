package main

import (
	"runtime"
	"strings"
)

// asciiFold rewrites the non-ASCII characters this CLI emits into ASCII
// equivalents.
//
// It exists because "the ASCII fallback" was not one. useEmoji() already
// returned false on every Windows console and on every redirected stream, but
// the strings taken on that path still carried em dashes and ellipses — the
// plain branch of welcomeBanner writes "Waired — connecting … inference…"
// literally. On a CP932 console those UTF-8 bytes were decoded as Shift_JIS
// and the whole `waired init` transcript came out as mojibake (#629). Emoji
// were never the problem; they had been degrading correctly for as long as
// emo() has existed.
//
// Runes with no entry here pass through unchanged, deliberately: a user's
// name, a device name and a model id can all be non-ASCII, and mangling those
// would be a worse bug than the one this fixes. The pure-ASCII test over the
// init renderers is what catches a newly introduced glyph that belongs in the
// table.
var asciiFolder = strings.NewReplacer(asciiFoldPairs()...)

// asciiFoldPairs is asciiFolder's argument list, in matching order.
//
// A function rather than one literal so the status marks can be a named list of
// their own: glyph_format_guard_test.go derives from statusMarkFolds the set of
// glyphs an fmt format string must not carry, and before waired-agent#1103 it
// kept a hand-written copy of them under a "kept in sync" comment with nothing
// checking it. A copy goes silently blind to a glyph added to only one side.
func asciiFoldPairs() []string {
	pairs := []string{
		// dashes and quotes
		"—", "-", // em dash — by far the most common, ~2600 sites
		"–", "-", // en dash
		"−", "-", // minus sign
		"·", "-", // middle dot, used as a separator
		"‘", "'", "’", "'",
		"“", `"`, "”", `"`,
		// ellipsis: three dots, not one, so "downloading…" still reads as ongoing
		"…", "...",
		// arrows and relations
		"→", "->",
		"←", "<-",
		"↔", "<->",
		"⇒", "=>",
		"≥", ">=",
		"≤", "<=",
		"≈", "~",
		"±", "+/-",
		"×", "x",
		// box drawing (style.go's rounded frames and rules)
		"─", "-",
		"│", "|",
		"╭", "+", "╮", "+", "╰", "+", "╯", "+",
		"█", "#",
		"▸", ">",
	}
	pairs = append(pairs, statusMarkFolds...)
	// a variation selector on its own renders as nothing
	return append(pairs, "️", "")
}

// statusMarkFolds are the status marks and their ASCII fallbacks. The
// emoji-bearing ones are normally resolved by emo() before they reach the fold;
// these entries are the backstop for the sites that hardcode a glyph.
//
// The variation-selector forms (U+FE0F) come first: a Replacer picks the
// pair listed earliest when two match at the same position, so "⚠"
// listed first would leave a bare selector behind.
//
// glyph_format_guard_test.go reads the folded-from halves of this list as the
// glyphs an fmt format string must not carry, so a mark added here is guarded
// from the same commit.
var statusMarkFolds = []string{
	"⚠️", "!", "⚠", "!",
	"⬇️", "*", "⬇", "*",
	"✅", "*",
	"✓", "*", "✔", "*",
	"✗", "x", "✕", "x",
	"●", "*", "◐", "*", "○", "o",
	"ℹ️", "i", "ℹ", "i",
	"🎉", "*",
	"🔌", "*",
	"⏳", "*",
	"⚡", "*",
}

func asciiFold(s string) string { return asciiFolder.Replace(s) }

// asciiOnlySink reports whether what this process writes will be decoded with
// a code page that cannot represent the bytes — the condition the fold exists
// for, and deliberately narrower than "glyphs are off".
//
// Only Windows qualifies, on two counts:
//
//   - a console whose output code page is not CP_UTF8 decodes the bytes as the
//     machine's ANSI page, which is the reported CP932 mojibake;
//   - anything redirected, whatever the console is set to. PowerShell decodes
//     a native command's stdout with its own cached Console.OutputEncoding —
//     fixed when that shell started, so a page this process sets afterwards
//     does not reach it. The rc8 transcript in #629 was captured through a
//     pipe, and `type`, Notepad and the Event Viewer read a saved log the same
//     ANSI way. A log a user can paste into a bug report is worth more than an
//     em dash.
//
// Unix is never folded: a pipe there is byte-transparent and a terminal
// follows the locale, so neither can turn UTF-8 into mojibake. `waired init |
// tee` on Linux keeps the em dashes it always had.
func asciiOnlySink(goos string, f glyphFacts) bool {
	if goos != "windows" {
		return false
	}
	return !f.consoleUTF8 || !f.stdoutTTY
}

var foldCached *bool

// foldOutput caches asciiOnlySink for the process, like useEmoji: main() sets
// the console code page before anything prints, so the answer cannot change
// mid-run.
func foldOutput() bool {
	if foldCached == nil {
		v := asciiOnlySink(runtime.GOOS, currentGlyphFacts())
		foldCached = &v
	}
	return *foldCached
}

// plainText folds s when the sink would mis-decode it, and leaves it alone
// otherwise. The one place the fold is applied to prose; emo() and the style.go
// helpers already resolve markers and frames through useEmoji().
func plainText(s string) string {
	if !foldOutput() {
		return s
	}
	return asciiFold(s)
}
