package main

import (
	"io"
	"os"
	"strings"
)

// ANSI styling + light box drawing for the init flow. Like emoji (emoji.go),
// this only renders on a capable TTY: piped/redirected output, NO_COLOR,
// WAIRED_NO_EMOJI, and non-UTF-8 locales fall back to plain text so CI logs,
// piped install logs, and `go test` output (the test binary's stdout is not a
// TTY) stay clean — which also keeps the existing substring assertions matching
// content that did not change.
//
// Two independent gates: useColor() controls ANSI SGR (bold/color); useEmoji()
// (emoji.go) controls whether we can draw Unicode box characters. NO_COLOR
// therefore drops color but keeps the boxes; WAIRED_NO_EMOJI (or a non-UTF-8
// locale) flattens everything to ASCII.

// useColor reports whether stdout can render ANSI SGR sequences. NO_COLOR (any
// value; https://no-color.org) or WAIRED_NO_EMOJI forces plain text; the result
// is cached after the first call (stdout's TTY-ness / locale don't change
// within a run), mirroring useEmoji.
func useColor() bool {
	if colorCached == nil {
		v := computeUseColor()
		colorCached = &v
	}
	return *colorCached
}

var colorCached *bool

func computeUseColor() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("WAIRED_NO_EMOJI") != "" { // one knob flattens emoji + color
		return false
	}
	if !isTerminal(os.Stdout) {
		return false
	}
	if !localeIsUTF8() {
		return false
	}
	enableVTProcessing() // no-op except on Windows conhost
	return true
}

// SGR building blocks. Kept private so the palette lives in one place and the
// useColor() gate is applied in exactly one spot (sgr).
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
	ansiRed     = "\x1b[31m"
)

// sgr wraps s in code + reset when color is on, else returns s unchanged.
func sgr(code, s string) string {
	if s == "" || !useColor() {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return sgr(ansiBold, s) }
func dim(s string) string    { return sgr(ansiDim, s) }
func green(s string) string  { return sgr(ansiGreen, s) }
func yellow(s string) string { return sgr(ansiYellow, s) }
func cyan(s string) string   { return sgr(ansiCyan, s) }

// product highlights a coding-agent product name (Claude Code / OpenCode /
// OpenClaw) so it stands out in the dense consent copy: bold + a stable
// per-product color. Plain when color is off, so "Claude Code skills",
// "OpenCode plugin", etc. stay contiguous for the substring assertions.
func product(name string) string {
	if !useColor() {
		return name
	}
	color := ansiCyan
	switch name {
	case "Claude Code":
		color = ansiMagenta
	case "OpenClaw":
		color = ansiYellow
	}
	return ansiBold + color + name + ansiReset
}

// rule is a faint full-width separator between major phases: dim box-drawing on
// a UTF-8 terminal, ASCII dashes otherwise. 60 cols stays under an 80-col
// terminal and under drawDownloadLine's 78-col pad.
func rule() string {
	ch := "-"
	if useEmoji() {
		ch = "─"
	}
	return dim(strings.Repeat(ch, 60))
}

// welcomeBanner prints the framed intro shown once at the very top of a fresh
// `waired init`. Fancy (UTF-8 TTY): a two-line rounded box. Plain: a single
// bold line, so redirected logs stay one grep-able line.
func welcomeBanner(out io.Writer) {
	sub := "connecting your coding agents to local inference…"
	if !useEmoji() {
		writePromptf(out, "%s — %s\n\n", bold("Waired"), sub)
		return
	}
	lines := []string{bold(cyan("W A I R E D")), dim(sub)}
	w := boxWidth(lines)
	writePrompt(out, boxTop(w))
	for _, l := range lines {
		writePrompt(out, boxRow(l, w))
	}
	writePrompt(out, boxBottom(w))
	writePrompt(out)
}

// box writes a framed summary: a blank line, then either a rounded Unicode box
// with the title in its top border (fancy) or a borderless rule + indented
// lines (plain). The caller passes an already-emoji-resolved marker (emo(...))
// and pre-styled detail lines. Off a UTF-8 TTY it degrades to plain text so
// install logs stay grep-able and box chars never leak. Keep caller lines
// ≤ ~66 display columns so the box fits an 80-column terminal.
func box(out io.Writer, marker, title string, lines []string) {
	header := marker + " " + bold(green(title))
	writePrompt(out)
	if !useEmoji() {
		writePrompt(out, rule())
		writePromptf(out, "%s\n", header)
		for _, l := range lines {
			writePrompt(out, "  "+l)
		}
		writePrompt(out, rule())
		return
	}
	hw := displayWidth(header)
	// Interior sizes to the widest content, but never smaller than the title
	// plus a dash on each side.
	w := max(boxWidth(lines), hw+2)
	// Top border with the title inset: ╭─ <header> ─…─╮ (interior = w+2 cols).
	rest := w - 1 - hw // dashes after the title; ≥1 given the hw+2 floor above
	top := dim("╭─") + " " + header + " " + dim(strings.Repeat("─", rest)+"╮")
	writePrompt(out, top)
	for _, l := range lines {
		writePrompt(out, boxRow(l, w))
	}
	writePrompt(out, boxBottom(w))
}

// boxWidth is the max display width across lines (the box interior sizes to the
// widest content).
func boxWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		if d := displayWidth(l); d > w {
			w = d
		}
	}
	return w
}

// boxTop / boxRow / boxBottom render the three row shapes of a rounded box with
// a 1-space pad on each side, so interior width is always w+2. Border chars are
// dimmed; the caller's content keeps its own styling.
func boxTop(w int) string    { return dim("╭" + strings.Repeat("─", w+2) + "╮") }
func boxBottom(w int) string { return dim("╰" + strings.Repeat("─", w+2) + "╯") }
func boxRow(l string, w int) string {
	return dim("│") + " " + l + strings.Repeat(" ", w-displayWidth(l)) + " " + dim("│")
}

// displayWidth returns the terminal column width of s: ANSI SGR sequences
// (\x1b[…m) contribute nothing, combining marks / variation selectors / ZWJ are
// zero-width, and wide runes (CJK, emoji) count as two. Good enough for the
// controlled strings the init boxes render (mostly ASCII + a leading emoji).
func displayWidth(s string) int {
	w := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' { // SGR terminator
				inEsc = false
			}
			continue
		}
		if r == 0x1b { // ESC — start of a CSI/SGR sequence
			inEsc = true
			continue
		}
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case (r >= 0x0300 && r <= 0x036F) || // combining diacritics
		(r >= 0xFE00 && r <= 0xFE0F) || // variation selectors (VS1–16)
		r == 0x200D: // zero-width joiner
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

// isWide reports whether r occupies two terminal columns. Covers the CJK blocks
// and the emoji / symbol ranges this CLI actually emits; not a full wcwidth.
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2600 && r <= 0x27BF, // misc symbols + dingbats (✅ ✔ …)
		r >= 0x2B00 && r <= 0x2BFF, // misc symbols & arrows (⬇ …)
		r >= 0x2E80 && r <= 0xA4CF, // CJK radicals … Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compat ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compat forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F000 && r <= 0x1FAFF, // emoji, symbols, pictographs
		r >= 0x20000 && r <= 0x3FFFD: // CJK ext-B and beyond
		return true
	}
	return false
}
