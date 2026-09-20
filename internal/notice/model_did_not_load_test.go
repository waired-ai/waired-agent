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
		// The wording is ratified, not a matter of taste (owner ruling,
		// 2026-09-21, recorded on waired-agent#1464), so it is pinned
		// verbatim. Two things in it were decided rather than drafted:
		//
		//   - "did not load", never "did not fit". `Fits` is the catalog's
		//     capacity verdict and this model PASSES it — saying it did not
		//     fit would read as that verdict having lied.
		//   - the alternative is "lighter and is what this computer would
		//     use instead", NOT "small enough to run here". The build that
		//     failed also passed the capacity check, so promising the
		//     replacement will load would repeat the same false claim.
		const wantTitle = "qwen3.5-122b-a10b did not load on this computer — switch to qwen3.8-flash-next"
		const wantText = "Waired ran out of memory loading qwen3.5-122b-a10b and stopped, and will " +
			"not load it again by itself. this computer ran out of memory putting the model in " +
			"memory. qwen3.8-flash-next is lighter and is what this computer would use instead."
		if n.Title != wantTitle {
			t.Errorf("title drifted from the ratified wording:\n got %s\nwant %s", n.Title, wantTitle)
		}
		if n.Text != wantText {
			t.Errorf("text drifted from the ratified wording:\n got %s\nwant %s", n.Text, wantText)
		}
		if strings.Contains(n.Title+" "+n.Text, "did not fit") {
			t.Error(`"did not fit" collides with the catalog's Fits verdict, which this model passes`)
		}
		if strings.Contains(n.Text, "small enough to run here") {
			t.Error("promising the alternative will load repeats the claim that just proved false")
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
