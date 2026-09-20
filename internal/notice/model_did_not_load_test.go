package notice

import (
	"strings"
	"testing"
)

// TestModelDidNotLoad covers the two shapes the notice has, and the thing
// that must be true of both.
//
// PRODUCT CONTRACT (waired-agent#1453): a build this computer could not load
// is SAID, whether or not there is something smaller to offer. Leaving it
// unsaid is how the product came to retry the same load forever — the
// warm-up logged "the first request will pay for it" and nothing reached a
// person.
func TestModelDidNotLoad(t *testing.T) {
	const (
		model   = "qwen3.5-122b-a10b"
		lighter = "qwen3.8-flash-next"
		reason  = "this computer ran out of memory putting the model in memory"
	)

	t.Run("with something smaller to offer", func(t *testing.T) {
		n := ModelDidNotLoad(model, lighter, reason)
		if n.Kind != KindModelDidNotLoad {
			t.Errorf("Kind = %q, want %q", n.Kind, KindModelDidNotLoad)
		}
		if n.Severity != SeverityWarn {
			t.Errorf("Severity = %v, want warn: `waired doctor` shows warnings, and a model "+
				"this computer cannot load is something the operator has to act on", n.Severity)
		}
		if n.Action != ActionModelSuggestion {
			t.Errorf("Action = %v, want a model suggestion so the tray can offer the switch", n.Action)
		}
		if n.Target != lighter {
			t.Errorf("Target = %q, want the model to switch to (%q)", n.Target, lighter)
		}
		for _, want := range []string{model, lighter} {
			if !strings.Contains(n.Title+" "+n.Text, want) {
				t.Errorf("neither the title nor the text names %q:\n  %s\n  %s", want, n.Title, n.Text)
			}
		}
	})

	t.Run("CONTRACT: with nothing smaller, it still says so", func(t *testing.T) {
		n := ModelDidNotLoad(model, "", reason)
		if n.Kind != KindModelDidNotLoad || n.Severity != SeverityWarn {
			t.Errorf("Kind/Severity = %q/%v, want the same notice as when there is an alternative",
				n.Kind, n.Severity)
		}
		if n.Action != ActionNone || n.Target != "" {
			t.Errorf("Action/Target = %v/%q, want none: there is nothing to switch to, and an "+
				"offer with no target is a button that does nothing", n.Action, n.Target)
		}
		if !strings.Contains(n.Title, model) {
			t.Errorf("title does not name the model that failed: %q", n.Title)
		}
		if !strings.Contains(n.Text, reason) {
			t.Errorf("text does not carry the reason: %q", n.Text)
		}
	})

	t.Run("it says the product will not keep trying", func(t *testing.T) {
		// The operator's next question after "it did not load" is "will it
		// try again". Answering it is the difference between this notice and
		// a log line.
		n := ModelDidNotLoad(model, lighter, reason)
		if !strings.Contains(n.Text, "will not load it again") {
			t.Errorf("text does not say the product has stopped trying: %q", n.Text)
		}
	})
}
