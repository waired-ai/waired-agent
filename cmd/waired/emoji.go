package main

import (
	"os"
	"runtime"
)

// emoji rendering for the init flow. Emoji make the (now installer-driven)
// first-run output friendlier, but they only render cleanly on a UTF-8
// terminal — piped/redirected output and non-UTF-8 consoles get an ASCII
// fallback so CI logs and minimal terminals stay readable.
//
// Which terminals count as UTF-8 is decided by glyphsSupported (console.go),
// which reads a Unix locale on Linux/macOS and the console output code page on
// Windows. Before #629 the Windows arm did not exist, so every Windows console
// took the ASCII path — including the Japanese ones this flow was mojibake on.

// useEmoji reports whether stdout can render emoji. Set WAIRED_NO_EMOJI=1
// to force the ASCII fallback. Result is cached after the first call
// (stdout's TTY-ness, the locale and the console code page don't change within
// a run — main() sets the code page before anything prints).
func useEmoji() bool {
	if emojiCached == nil {
		v := computeUseEmoji()
		emojiCached = &v
	}
	return *emojiCached
}

var emojiCached *bool

func computeUseEmoji() bool {
	// The TTY gate is not OS-varying, so it stays out of glyphsSupported and
	// out of its table test; slGlyph deliberately skips it because the
	// statusline's stdout is a pipe to Claude Code.
	if !isTerminal(os.Stdout) {
		return false
	}
	return glyphsSupported(runtime.GOOS, currentGlyphFacts())
}

// resetGlyphCacheForTest clears the process-wide useEmoji/useColor caches so a
// test can set the environment and observe the effect. Both are computed once
// per process in production; without this seam whichever test ran first would
// pin the value for the rest of the package (CLAUDE.md §Test discipline).
func resetGlyphCacheForTest() {
	emojiCached = nil
	colorCached = nil
}

// emo returns symbol when emoji are supported, else the ASCII fallback.
func emo(symbol, fallback string) string {
	if useEmoji() {
		return symbol
	}
	return fallback
}
