package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/router"
)

// The two refusals waired-agent#1395 gave their own answer on both listeners:
// a model row's window floor that no computer meets, and a pinned computer a
// filter rules out. Both are a 400 — nothing clears them without someone
// picking another row or changing a computer — and both used to be something
// else: the window refusal a 500 that Claude Code retries ten times, the pin
// a quiet run on another computer.
//
// The sentences deliberately avoid every phrase a coding tool reads as "the
// prompt is too long for this model" — OpenCode compacts the conversation and
// retries on those, and that would loop on a refusal no prompt length can fix.
// TestWindowRefusalsAreNotReadAsContextOverflow holds them to OpenCode's own
// patterns.

// windowFloorDetail is the sentence for a window refusal. The Selector always
// returns a *router.WindowFloorError; the bare sentinel gets the sentence
// without the numbers.
func windowFloorDetail(err error) string {
	e, ok := router.WindowFloor(err)
	switch {
	case !ok || e.Need <= 0:
		return "None of your computers that could take this turn has the context window the model you picked needs"
	case e.Local && e.LocalWindow <= 0:
		return fmt.Sprintf("This computer does not state a context window, and the model you picked needs %d tokens", e.Need)
	case e.Local:
		return fmt.Sprintf("This computer's context window is %d tokens, and the model you picked needs %d", e.LocalWindow, e.Need)
	case e.Public:
		return fmt.Sprintf("No public computer that could take this turn has a context window of %d tokens, which the model you picked needs", e.Need)
	}
	return fmt.Sprintf("None of your computers that could take this turn has a context window of %d tokens, which the model you picked needs. A model row that names one computer does not ask for this", e.Need)
}

// pinnedPeerDeclinedDetail is the sentence for a declined pin.
func pinnedPeerDeclinedDetail(err error) string {
	if e, ok := router.PinnedPeerDeclined(err); ok {
		return e.Error()
	}
	return "The computer this turn is pinned to cannot take it"
}

// stageWindowFloorHeaders names the refusal and the floor for the journal and
// a support capture, on both listeners.
func stageWindowFloorHeaders(w http.ResponseWriter, err error) {
	w.Header().Set(HeaderLocalError, LocalErrorNoComputerForWindow)
	if e, ok := router.WindowFloor(err); ok && e.Need > 0 {
		w.Header().Set(HeaderRequiredWindow, strconv.Itoa(e.Need))
	}
}

// stagePinnedPeerDeclinedHeaders is the same for a declined pin.
func stagePinnedPeerDeclinedHeaders(w http.ResponseWriter, err error) {
	w.Header().Set(HeaderLocalError, LocalErrorPinnedPeerDeclined)
	e, ok := router.PinnedPeerDeclined(err)
	if !ok {
		return
	}
	if e.PeerDisplayID != "" {
		w.Header().Set(HeaderInferencePeer, e.PeerDisplayID)
	}
	if e.Reason == router.PinDeclinedWindow {
		w.Header().Set(HeaderRequiredWindow, strconv.Itoa(e.Need))
	}
}

// sentence stops a detail for a surface that adds nothing after it.
func sentence(detail string) string {
	if strings.HasSuffix(detail, ".") {
		return detail
	}
	return detail + "."
}
