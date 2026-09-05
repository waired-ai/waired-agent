// Package notice carries short user-facing messages from the daemon to
// the surfaces that show them: the system tray, `waired doctor` and
// `waired status`.
//
// A notice is a message, not a command. It says what is true, names at
// most one thing a person can act on, and carries no payload the
// renderer has to understand — the surfaces render it, they do not
// interpret it.
//
// The prose is composed HERE. Producers call a typed constructor per
// kind and never supply a string, so a value a renderer would misread
// cannot be built rather than being refused at publish time. Refusing
// would drop the notice silently, and a notice nobody sees is the defect
// this package exists to fix (waired-agent#1205).
//
// The JSON tags on Notice are a wire contract: they are read back by a
// `waired` CLI that may be older or newer than the daemon that wrote
// them. Keep this package on the standard library so it never drags a
// dependency into the daemon's serialization path.
package notice

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MaxActive is the largest number of notices any surface has to render.
// The registry clamps to it, so a renderer can pre-allocate exactly this
// many rows and never needs an overflow row: the system tray cannot add
// menu items after startup (internal/gui/tray/tray.go), which is what
// makes a fixed, shared cap worth having rather than a per-surface one.
const MaxActive = 5

// maxRunes bounds every composed string. A menu label is truncated by
// the OS and the Windows status dialog does not scroll
// (internal/gui/tray/status_report.go), so a long notice is not a
// display problem to solve later — it is one to prevent here.
const maxRunes = 160

// Kind identifies what a notice is about. It is stable wire text: a
// surface may branch on it, and an older CLI must be able to ignore one
// it has never heard of.
type Kind string

const (
	// KindLighterModel is the #133 step-down suggestion: this computer
	// measured below the interactive floor with the model it runs.
	KindLighterModel Kind = "lighter_model"
	// KindBetterModel is the step-up suggestion: this computer has the
	// headroom for a higher tier than the model it runs.
	KindBetterModel Kind = "better_model"
)

// Severity says how a surface should mark a notice, and whether a
// surface that only reports defects should show it at all.
type Severity int

const (
	// SeverityInfo is worth reading and is not a problem. `waired
	// doctor` does not show these: it reports on the health of the
	// setup, and "you could run a better model" is not a fault in it.
	SeverityInfo Severity = iota
	// SeverityWarn is working, but not the way it should be — the same
	// meaning `waired doctor`'s ⚠ already carries.
	SeverityWarn
)

// Action names what a person can do about a notice. It is an enum the
// renderer interprets, deliberately not a payload: the tray resolves the
// details it needs from state it already keeps, so a general-purpose
// message never has to grow a typed field for one producer.
type Action int

const (
	// ActionNone is a notice with nothing to click.
	ActionNone Action = iota
	// ActionModelSuggestion offers to switch the model this computer
	// runs. The tray opens the accept/decline dialog it already has.
	ActionModelSuggestion
)

// Notice is one message. Every string field has been through sanitise —
// on construction here, and again on the way back off the wire — so a
// renderer can print it without escaping it for anything but its own
// markup dialect (which, for the tray, happens once in
// internal/gui/tray/rows.go).
type Notice struct {
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	// Title is the short form: one menu row, one doctor subject.
	Title string `json:"title"`
	// Text is the sentence behind the title — the figures a person
	// needs to judge it. May be empty.
	Text string `json:"text,omitempty"`
	// Action and Target are what a surface offers to do about it.
	// Target is an opaque display string, not an identifier a renderer
	// is expected to resolve.
	Action Action `json:"action,omitempty"`
	Target string `json:"target,omitempty"`

	// FirstSeen is when this notice first appeared, carried forward
	// across re-publishes so the render order does not reshuffle every
	// time a producer repeats itself. ExpiresAt is when it lapses if
	// nobody republishes. Neither crosses the wire: the daemon has
	// already applied both by the time a surface reads the list, and
	// publishing them would invite a consumer to re-implement the
	// policy.
	FirstSeen time.Time `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

// LighterModel is the #133 suggestion to step down. measured and floor
// are tok/s; from and to are model ids.
func LighterModel(from, to string, measured, floor float64) Notice {
	return Notice{
		Kind:     KindLighterModel,
		Severity: SeverityWarn,
		Title:    sanitise("Lighter model recommended — switch to " + to),
		Text: sanitise("This computer answers at " + tokps(measured) + " with " + from +
			", below the " + tokps(floor) + " floor."),
		Action: ActionModelSuggestion,
		Target: sanitise(to),
	}
}

// BetterModel is the suggestion to step up. measured is what this
// computer manages with the model it runs; predicted is the estimate for
// the suggested one.
func BetterModel(from, to string, measured, predicted float64) Notice {
	return Notice{
		Kind:     KindBetterModel,
		Severity: SeverityInfo,
		Title:    sanitise("Better model available — switch to " + to),
		Text: sanitise("This computer answers at " + tokps(measured) + " with " + from +
			"; " + to + " should manage about " + tokps(predicted) + " here."),
		Action: ActionModelSuggestion,
		Target: sanitise(to),
	}
}

// UnmarshalJSON decodes a notice and puts every string field through the
// same sanitiser the constructors use.
//
// The constructors protect the producer; this protects the reader. All
// three surfaces decode this type off a local socket and render the
// result, so the invariant has to hold on both sides of the wire — a
// daemon that is newer, older, or simply wrong must not be able to hand
// a terminal an escape sequence or a menu a control character. Unknown
// Severity and Action values are clamped rather than rejected, so a
// notice from a newer daemon still renders as an ordinary one.
func (n *Notice) UnmarshalJSON(b []byte) error {
	type wire Notice
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*n = Notice(w)
	n.Kind = Kind(sanitise(string(n.Kind)))
	n.Title = sanitise(n.Title)
	n.Text = sanitise(n.Text)
	n.Target = sanitise(n.Target)
	if n.Severity != SeverityWarn {
		n.Severity = SeverityInfo
	}
	if n.Action != ActionModelSuggestion {
		n.Action = ActionNone
	}
	return nil
}

// Sanitise is sanitise, exported for the guard in cmd/waired that holds
// this package against that command's own status-mark table. The table
// lives in package main and cannot be imported here, so the two are kept
// in step by a test rather than by a second copy of the list.
func Sanitise(s string) string { return sanitise(s) }

// sanitise reduces a string to something every renderer can print as one
// line of text.
//
// It removes, in order of how badly each one goes wrong:
//
//   - C0/C1 controls, which includes ESC — `waired doctor` prints to a
//     terminal, and a terminal EXECUTES an escape sequence rather than
//     showing it;
//   - newlines, which break the one-line contract of a doctor finding,
//     a status line and a menu label;
//   - Unicode bidirectional and other format controls (U+202A-U+202E,
//     U+2066-U+2069, U+FEFF …), which visually reorder a line so it
//     reads as something other than what it says;
//   - the status marks the surfaces use to say how a line should be
//     read — a notice carrying ✓ or ✗ would forge one;
//   - runs of whitespace, and anything past maxRunes.
//
// It does NOT escape menu markup. That is per-renderer and is applied
// once, at the widget (internal/gui/tray/rows.go); doing it here would
// put markup into the status report, the clipboard and the debug dump,
// none of which are menus.
func sanitise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			b.WriteRune('…')
			break
		}
		switch {
		case unicode.Is(unicode.Cf, r):
			// Bidi and other format controls: drop without even the
			// word break, so removing one cannot split a word.
			continue
		case unicode.IsControl(r), unicode.IsSpace(r):
			space = true
			continue
		case statusMark(r), !unicode.IsPrint(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteRune(' ')
			n++
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// statusMark reports whether r is one of the marks a surface uses to say
// how a line should be read. Keeping notices free of them is what stops
// a message forging a verdict — an "✓ everything is fine" doctor row, or
// a green dot in a menu.
//
// The authoritative list for the CLI is statusMarkFolds in
// cmd/waired/ascii.go. It cannot be imported (package main), so a test
// over there asserts every rune in it is removed by this function. That
// test fails when the table grows, which a second copy of the list here
// would not.
func statusMark(r rune) bool {
	switch r {
	case '⚠', '✅', '✓', '✔', '✗', '✕', '●', '◐', '○', '◦', 'ℹ', 'ⓘ',
		'⬆', '⬇', '↓', '↑', '•', '·', '⋯', '🎉', '🔌', '⏳', '⏱', '⚡',
		'🐢', '🌐', '📦', '🤖':
		return true
	}
	return false
}

// tokps renders a throughput figure the way every other surface that
// quotes one already does: whole tok/s, no decimal (the %.0f in
// cmd/waired/init_benchmark.go, which prints the same measurement to the
// same person during setup). A notice that rounded differently from the
// setup line would read as a second, disagreeing measurement.
func tokps(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + " tok/s" }
