package router

import (
	"errors"
	"fmt"
)

// WindowFloorError is ErrNoEndpointForWindow as the Selector returns it: a
// request carried a window floor (Request.MinContextWindow), and every
// computer that could otherwise have taken it was removed by that floor.
//
// It is its own answer rather than a shade of "no host serves this model".
// Before waired-agent#1395 only the local-only arm produced the sentinel, and
// a floor that emptied the mesh came back as the generic mesh miss, which
// names neither the window nor the row that asked for it — and, having no
// arm of its own on either listener, the sentinel itself was a 500 that
// Claude Code retries ten times.
type WindowFloorError struct {
	// Need is the window the request demanded.
	Need int
	// Local is set when the refusal is about THIS computer alone — the
	// local-only arm, whose row names this computer — and LocalWindow is
	// what it declares (0 = nothing).
	Local       bool
	LocalWindow int
	// Public is set when the request asked for someone else's computer
	// ("Waired public share"), so a surface does not send the reader to
	// their own machines.
	Public bool
}

func (e *WindowFloorError) Error() string {
	if e.Local {
		if e.LocalWindow <= 0 {
			return fmt.Sprintf("router: this computer states no context window, and this request needs %d tokens", e.Need)
		}
		return fmt.Sprintf("router: this computer's context window is %d tokens, and this request needs %d", e.LocalWindow, e.Need)
	}
	if e.Public {
		return fmt.Sprintf("router: no public computer that could take this request has a context window of %d tokens", e.Need)
	}
	return fmt.Sprintf("router: no computer that could take this request has a context window of %d tokens", e.Need)
}

func (e *WindowFloorError) Unwrap() error { return ErrNoEndpointForWindow }

// WindowFloor returns the typed window refusal behind err, if that is what
// it is.
func WindowFloor(err error) (*WindowFloorError, bool) {
	var e *WindowFloorError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Why a pin was declined. Strings, because they travel to the event ring and
// the X-Waired-Local-Error detail as they are.
const (
	// PinDeclinedWindow: the computer's declared window is under the floor
	// the row demands.
	PinDeclinedWindow = "window"
	// PinDeclinedServeMain / PinDeclinedServeSub: the computer's owner turned
	// off serving that kind of turn.
	PinDeclinedServeMain = "serve_main_off"
	PinDeclinedServeSub  = "serve_sub_off"
	// PinDeclinedPublicShare: a public machine the consumer's Public Share
	// posture does not admit.
	PinDeclinedPublicShare = "public_share"
	// PinDeclinedUnknownModel: the computer serves nothing this build's
	// catalog knows, so there is no model to give it.
	PinDeclinedUnknownModel = "unknown_model"
)

// PinnedPeerDeclinedError is ErrPinnedPeerDeclined with the computer and the
// reason named.
//
// PeerDisplayID follows Selection.PeerDisplayID: the grant pseudonym for a
// Public Share machine, never its real device id (spec §8.5).
type PinnedPeerDeclinedError struct {
	PeerDisplayID string
	PeerName      string
	ModelID       string
	Reason        string
	// Need / Declared are the floor and the computer's declared window, set
	// when Reason is PinDeclinedWindow.
	Need     int
	Declared int
}

func (e *PinnedPeerDeclinedError) Error() string {
	// Worded like the pin's unreachable refusal ("The computer this turn is
	// pinned to, rtx4000-linux, is not answering"), which is also what keeps a
	// computer's name from being capitalised by the Claude surface's
	// sentence-casing.
	who := e.PeerName
	if who == "" {
		who = e.PeerDisplayID
	}
	subject := "The computer this turn is pinned to"
	if who != "" {
		subject += ", " + who + ","
	}
	var why string
	switch e.Reason {
	case PinDeclinedWindow:
		if e.Declared <= 0 {
			why = fmt.Sprintf("it does not state a context window, and the model you picked needs %d tokens", e.Need)
		} else {
			why = fmt.Sprintf("its context window is %d tokens, and the model you picked needs %d", e.Declared, e.Need)
		}
	case PinDeclinedServeMain:
		why = "Serve main conversation is turned off on it"
	case PinDeclinedServeSub:
		why = "Serve subagents is turned off on it"
	case PinDeclinedPublicShare:
		why = "your Public Share settings do not let this computer use it"
	case PinDeclinedUnknownModel:
		why = "it is running a model this version of Waired does not know"
	default:
		why = "it cannot take this kind of turn"
	}
	return fmt.Sprintf("%s cannot take it: %s", subject, why)
}

func (e *PinnedPeerDeclinedError) Unwrap() error { return ErrPinnedPeerDeclined }

// PinnedPeerDeclined returns the typed pin refusal behind err, if that is
// what it is.
func PinnedPeerDeclined(err error) (*PinnedPeerDeclinedError, bool) {
	var e *PinnedPeerDeclinedError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// pinDeclineReason reads why a one-peer buildMeshCandidates pass returned
// nothing. The narrowed snapshot holds only the pin, so each counter is
// either 0 or about the pin itself. The size floor is not here: a pin under
// the operator's routing floor is named by SizeFloorError, which says which
// setting to change.
//
// "" means no filter fired — the pin serves nothing the catalog knows.
func pinDeclineReason(d meshDrops) string {
	switch {
	case d.belowWindow > 0:
		return PinDeclinedWindow
	case d.excludedMain > 0:
		return PinDeclinedServeMain
	case d.excludedSub > 0:
		return PinDeclinedServeSub
	case d.publicDeclined > 0 || d.belowPublicFloor > 0:
		return PinDeclinedPublicShare
	}
	return ""
}
