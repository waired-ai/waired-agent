package tray

import (
	"fmt"
	"strings"
	"testing"

	"fyne.io/systray"
)

// fakeRow records the real mutation calls in order. Order matters as much as
// membership here: the defect these tests pin is a mutation arriving at a row
// that is not on the menu.
type fakeRow struct {
	ops []string
}

func (f *fakeRow) SetTitle(s string)   { f.ops = append(f.ops, "SetTitle("+s+")") }
func (f *fakeRow) SetTooltip(s string) { f.ops = append(f.ops, "SetTooltip("+s+")") }
func (f *fakeRow) Enable()             { f.ops = append(f.ops, "Enable()") }
func (f *fakeRow) Disable()            { f.ops = append(f.ops, "Disable()") }
func (f *fakeRow) Show()               { f.ops = append(f.ops, "Show()") }
func (f *fakeRow) Hide()               { f.ops = append(f.ops, "Hide()") }

func (f *fakeRow) got() string { return strings.Join(f.ops, " ") }

// newRowPass returns a tray with an open (unforced) diff pass, which is the
// state every diff helper is called in.
func newRowPass(t *testing.T) *tray {
	t.Helper()
	tr := &tray{}
	tr.beginRowPass(false)
	return tr
}

// PRODUCT CONTRACT (#317): a row the current model hides receives Hide() and
// nothing else. On fyne.io/systray's Windows backend SetTitle / SetTooltip /
// Enable / Disable re-insert a hidden item into the menu, so any of them
// leaking through is a resurrected blank row on a user's screen.
func TestSetRow_HiddenRowTakesOnlyHide(t *testing.T) {
	tr := newRowPass(t)
	row := &fakeRow{}

	// Row was visible with a title, and the new model drops it entirely —
	// the shape of every group that disappears when the daemon goes down.
	tr.setVisible(row, true, false)
	tr.setTitle(row, "Models", "Models")
	tr.setTitle(row, "Models", "")
	tr.setTooltip(row, "pick a model", "")
	tr.setEnabled(row, true, false)

	if got := row.got(); got != "Hide()" {
		t.Errorf("hidden row: got %q, want only Hide()", got)
	}
}

// A row that stays hidden across passes must stay quiet too: the daemon-down
// menu repaints every 5 s, and the resurrect bug fired on the repaint, not
// only on the transition.
func TestSetRow_StaysHiddenAcrossPasses(t *testing.T) {
	tr := newRowPass(t)
	row := &fakeRow{}
	tr.setVisible(row, true, false)
	row.ops = nil

	for i := range 3 {
		tr.beginRowPass(false)
		tr.setVisible(row, false, false)
		tr.setTitle(row, "", fmt.Sprintf("Models %d", i))
		tr.setEnabled(row, false, true)
	}

	if got := row.got(); got != "" {
		t.Errorf("row hidden in every pass: got %q, want no mutations", got)
	}
}

// The mirror of the rule above: while a row is hidden its title diffs are
// dropped, so the widget's title is stale by the time it reappears. Showing it
// must push the title even though prev == next.
func TestSetRow_ShowRepushesUnchangedTitle(t *testing.T) {
	tr := newRowPass(t)
	row := &fakeRow{}

	// Hidden pass: the model changes the label, the widget never hears it.
	tr.setVisible(row, true, false)
	tr.setTitle(row, "Peers: 1", "Peers: 2")
	row.ops = nil

	// Next pass shows it again with that same (to the model, unchanged) label.
	tr.beginRowPass(false)
	tr.setVisible(row, false, true)
	tr.setTitle(row, "Peers: 2", "Peers: 2")
	tr.setEnabled(row, true, true)

	if got := row.got(); got != "Show() SetTitle(Peers: 2) Enable()" {
		t.Errorf("row reappearing: got %q, want the title (and enablement) re-pushed", got)
	}
}

// shown is per-pass state: the pass after the one that revealed a row must go
// back to diffing, or every visible row re-pushes its title on every poll —
// the DBus chatter setTitle exists to avoid.
func TestSetRow_SteadyStateIsQuiet(t *testing.T) {
	tr := newRowPass(t)
	row := &fakeRow{}
	tr.setVisible(row, false, true)
	tr.setTitle(row, "", "Connected")
	row.ops = nil

	tr.beginRowPass(false)
	tr.setVisible(row, true, true)
	tr.setTitle(row, "Connected", "Connected")
	tr.setTooltip(row, "", "")
	tr.setEnabled(row, true, true)

	if got := row.got(); got != "" {
		t.Errorf("unchanged visible row: got %q, want no mutations", got)
	}
}

// Rows no visibility diff ever touches — miHeader, miSettings, miAbout,
// miAutostart, miQuit — are permanently on the menu, so their titles must
// still be pushed even though they never enter rowStates.
func TestSetRow_UntrackedRowIsTreatedAsVisible(t *testing.T) {
	tr := newRowPass(t)
	row := &fakeRow{}

	tr.setTitle(row, "◐ Connecting…", "● Connected")

	if got := row.got(); got != "SetTitle(● Connected)" {
		t.Errorf("untracked row: got %q, want the title pushed", got)
	}
}

// paintCreationBaseline's mechanism: systray creates items visible and the
// zero-model-against-zero-model diff says "nothing changed", so the pass has
// to push anyway. Without force the row stays visible and blank (#317).
func TestSetRow_ForcedPassAssertsVisibility(t *testing.T) {
	hidden, shown := &fakeRow{}, &fakeRow{}

	tr := &tray{}
	tr.beginRowPass(true)
	tr.setVisible(hidden, false, false)
	tr.setVisible(shown, true, true)
	tr.rowForce = false

	if got := hidden.got(); got != "Hide()" {
		t.Errorf("forced pass, invisible row: got %q, want Hide()", got)
	}
	if got := shown.got(); got != "Show()" {
		t.Errorf("forced pass, visible row: got %q, want Show()", got)
	}
}

// The old helpers guarded on `mi == nil`. Boxing a *systray.MenuItem in an
// interface breaks that comparison, and a nil item would panic instead —
// which is why nilRow exists rather than a plain != nil.
func TestNilRow(t *testing.T) {
	var unset *systray.MenuItem

	cases := map[string]struct {
		row  menuRow
		want bool
	}{
		"nil interface":     {nil, true},
		"nil menu item":     {unset, true},
		"allocated fake":    {&fakeRow{}, false},
		"allocated by menu": {&systray.MenuItem{}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := nilRow(c.row); got != c.want {
				t.Errorf("nilRow(%s) = %v, want %v", name, got, c.want)
			}
		})
	}

	// The case that matters: an item onReady has not assigned yet must not
	// reach the widget. Driven through the helpers, so a panic is the failure.
	tr := newRowPass(t)
	tr.setVisible(unset, false, true)
	tr.setTitle(unset, "", "x")
	tr.setTooltip(unset, "", "x")
	tr.setEnabled(unset, false, true)
}
