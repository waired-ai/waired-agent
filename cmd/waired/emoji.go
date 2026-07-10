package main

import (
	"os"
	"strings"
)

// emoji rendering for the init flow. Emoji make the (now installer-driven)
// first-run output friendlier, but they only render cleanly on a UTF-8
// terminal — piped/redirected output and non-UTF-8 locales get an ASCII
// fallback so CI logs and minimal terminals stay readable.

// useEmoji reports whether stdout can render emoji. Set WAIRED_NO_EMOJI=1
// to force the ASCII fallback. Result is cached after the first call
// (stdout's TTY-ness and the locale don't change within a run).
func useEmoji() bool {
	if emojiCached == nil {
		v := computeUseEmoji()
		emojiCached = &v
	}
	return *emojiCached
}

var emojiCached *bool

func computeUseEmoji() bool {
	if os.Getenv("WAIRED_NO_EMOJI") != "" {
		return false
	}
	if !isTerminal(os.Stdout) {
		return false
	}
	return localeIsUTF8()
}

func localeIsUTF8() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			u := strings.ToUpper(v)
			return strings.Contains(u, "UTF-8") || strings.Contains(u, "UTF8")
		}
	}
	return false
}

// emo returns symbol when emoji are supported, else the ASCII fallback.
func emo(symbol, fallback string) string {
	if useEmoji() {
		return symbol
	}
	return fallback
}
