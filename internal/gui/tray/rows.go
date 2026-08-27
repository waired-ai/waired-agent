package tray

import (
	"runtime"

	"fyne.io/systray"
)

// The row diff. Every mutation of a pre-allocated menu item goes through the
// four *tray methods below, and they enforce two invariants:
//
//	nothing but Hide() is ever sent to a row the current model leaves hidden,
//	and a row the model reveals gets its title BEFORE it appears.
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
// The second invariant is the same Windows mechanism read forwards. Show() is
// addOrUpdateMenuItem too, and it carries the item's LAST STORED title — so a
// row revealed before its title is pushed is inserted with whatever string it
// held while hidden: the empty one it was created with, or the label from the
// last time it was on the menu. The correct title arrives on the very next
// Win32 call, which is why the artifact is one frame of a blank (or stale) row
// rather than a permanent one (#351). Rows therefore record the reveal, take
// their title and tooltip while systray still considers them hidden, and are
// shown together by endRowPass at the end of the pass. Insert POSITION is
// derived from the item id, not from call order (systray_windows.go's
// addToVisibleItems sorts), so deferring the Show() cannot reorder the menu.
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
	// pendingShow marks a reveal endRowPass still owes the widget. It is
	// set instead of calling Show() so the title lands first (#351); it is
	// distinct from shown, which stays true for the rest of the pass to keep
	// forcing the diffs.
	pendingShow bool
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
//
// A reveal is recorded rather than performed: from here on the row counts as
// visible, so its title and tooltip are pushed by the diffs below, and
// endRowPass raises it once it has them.
func (t *tray) setVisible(mi menuRow, prev, next bool) {
	if nilRow(mi) {
		return
	}
	st := t.rowStates[mi]
	if prev != next || t.rowForce {
		if next {
			st.shown = true
			st.pendingShow = true
		} else {
			mi.Hide()
			st.pendingShow = false
		}
	}
	st.visible = next
	t.rowStates[mi] = st
}

// endRowPass raises every row this pass revealed, now that each one is
// carrying the title and tooltip the model asked for. Called from the end of
// diffRows rather than from the two callers of beginRowPass, so a third caller
// cannot forget it and leave a row that the model shows sitting invisible.
//
// Map iteration order is unspecified, and it does not decide POSITION: a
// backend derives that from the item id, not from the order the Show() calls
// arrive in. It does decide which Show() lands first, though, and on the
// Windows backend that used to decide whether a submenu child was inserted
// at all — see the submenu-parent note on onReady (waired-agent#1063). The
// invariant that keeps this loop order-free is held there, at construction:
// a parent owns a real submenu handle before anything is hidden, so a child
// Show() can never be the call that has to create one.
func (t *tray) endRowPass() {
	for mi, st := range t.rowStates {
		if !st.pendingShow {
			continue
		}
		mi.Show()
		st.pendingShow = false
		t.rowStates[mi] = st
	}
}

// setTitle avoids the systray DBus chatter that SetTitle on every poll would
// produce, and skips hidden rows entirely (see the file comment).
//
// It is also where every dynamic label is escaped for the OS drawing it
// (menulabel.go). This is the last point before the label leaves Go, and
// that placement is load-bearing: a device name, an ollama tag or an
// engine's LastError reaches some fifty rows through here, and escaping
// any earlier — in the MenuModel — would put the markup in the status
// report, the clipboard and the debug dump as well, none of which are
// menus.
//
// prev/next are compared UNESCAPED, so the suppression still keys on
// whether the label actually changed.
func (t *tray) setTitle(mi menuRow, prev, next string) {
	if nilRow(mi) {
		return
	}
	visible, shown := t.rowVisible(mi)
	if !visible || (prev == next && !shown) {
		return
	}
	mi.SetTitle(escapeMenuLabel(runtime.GOOS, next))
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
