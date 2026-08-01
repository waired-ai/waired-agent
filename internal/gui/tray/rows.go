package tray

import "fyne.io/systray"

// The row diff. Every mutation of a pre-allocated menu item goes through the
// four *tray methods below, and they enforce one invariant:
//
//	nothing but Hide() is ever sent to a row the current model leaves hidden.
//
// That invariant is not cosmetic. On fyne.io/systray v1.12.2's Windows
// backend, Hide() is RemoveMenu + delFromVisibleItems, while SetTitle /
// SetTooltip / Enable / Disable all funnel into addOrUpdateMenuItem — which,
// for an item that is no longer in visibleItems, skips SetMenuItemInfo and
// falls into the InsertMenuItem branch (systray_windows.go:605-639). In other
// words, on Windows a SetTitle on a hidden row is Show() by another name. That
// is how the daemon-down menu grew two blank rows and enabled-but-empty
// "Models" / "Claude Code" parents (#317): apply() hid them, then pushed a
// fallback title at them in the next statement.
//
// Linux (dbusmenu keeps a separate "visible" property) and macOS
// (-[NSMenuItem setHidden:]) are immune to the re-insert, but the suppression
// here is unconditional so all three backends are driven identically — and so
// a future reviewer does not have to know which backend forgives what.
//
// The second half of the fix is paintCreationBaseline: systray creates every
// item visible, apply() diffs model-to-model, and a (false,false) visibility
// diff is a no-op — so a row that nobody remembered to Hide() at creation sat
// on the menu blank forever. Deriving the creation state from the zero
// MenuModel instead of from ~40 hand-written Hide() calls makes that class of
// drift impossible rather than merely fixed once (waired#808 fixed miStatus /
// miPeers by hand; miEmail, miToggle, miAdmin, miLogout and the whole
// OpenCode / OpenClaw / device group were missed).

// menuRow is the mutation surface of one pre-allocated systray item.
// *systray.MenuItem satisfies it; rows_test.go substitutes a recorder, which
// is the only way to assert the invariant above without a live tray session
// (the tray's own fields stay *systray.MenuItem because click dispatch needs
// the exported ClickedCh field, which no interface can carry).
type menuRow interface {
	SetTitle(string)
	SetTooltip(string)
	Enable()
	Disable()
	Show()
	Hide()
}

// rowState is what the last diff pass left on a row.
type rowState struct {
	visible bool
	// shown marks a row that THIS pass turned visible. The title / tooltip /
	// enabled diffs that follow have to push unconditionally in that case:
	// while the row was hidden every such mutation was suppressed, so
	// prev == next no longer proves the widget already agrees.
	shown bool
}

// nilRow reports whether mi is absent. A typed nil pointer boxed in an
// interface is not == nil, and every caller passes a concrete
// *systray.MenuItem, so the plain comparison alone would not catch the
// not-yet-created case the old helpers guarded against.
func nilRow(mi menuRow) bool {
	if mi == nil {
		return true
	}
	p, ok := mi.(*systray.MenuItem)
	return ok && p == nil
}

// beginRowPass opens a diff pass. force makes every visibility decision push
// to the widget even when the model-to-model diff says nothing changed; it is
// how paintCreationBaseline asserts the initial state, since there the diff is
// zero-model against zero-model and would otherwise be one long no-op.
//
// Callers hold t.applyMu for the whole pass, which is also what makes
// rowStates safe: apply() runs on the poll goroutine AND on every click
// handler that calls pollOnce, and concurrent writes to a map panic.
func (t *tray) beginRowPass(force bool) {
	t.rowForce = force
	if t.rowStates == nil {
		t.rowStates = make(map[menuRow]rowState)
		return
	}
	for mi, st := range t.rowStates {
		st.shown = false
		t.rowStates[mi] = st
	}
}

// rowVisible reports the row's current visibility, and whether this pass is
// what made it visible. Rows no visibility diff ever touches — miHeader,
// miSettings, miAbout, miAutostart, miQuit — never enter the map and are
// reported visible, which is exactly what they are.
func (t *tray) rowVisible(mi menuRow) (visible, shown bool) {
	st, ok := t.rowStates[mi]
	if !ok {
		return true, false
	}
	return st.visible, st.shown
}

// setVisible is the visibility half of the row diff, and the only place a row
// is allowed to appear or disappear.
func (t *tray) setVisible(mi menuRow, prev, next bool) {
	if nilRow(mi) {
		return
	}
	st := t.rowStates[mi]
	if prev != next || t.rowForce {
		if next {
			mi.Show()
			st.shown = true
		} else {
			mi.Hide()
		}
	}
	st.visible = next
	t.rowStates[mi] = st
}

// setTitle avoids the systray DBus chatter that SetTitle on every poll would
// produce, and skips hidden rows entirely (see the file comment).
func (t *tray) setTitle(mi menuRow, prev, next string) {
	if nilRow(mi) {
		return
	}
	visible, shown := t.rowVisible(mi)
	if !visible || (prev == next && !shown) {
		return
	}
	mi.SetTitle(next)
}

func (t *tray) setTooltip(mi menuRow, prev, next string) {
	if nilRow(mi) {
		return
	}
	visible, shown := t.rowVisible(mi)
	if !visible || (prev == next && !shown) {
		return
	}
	mi.SetTooltip(next)
}

func (t *tray) setEnabled(mi menuRow, prev, next bool) {
	if nilRow(mi) {
		return
	}
	visible, shown := t.rowVisible(mi)
	if !visible || (prev == next && !shown) {
		return
	}
	if next {
		mi.Enable()
	} else {
		mi.Disable()
	}
}
