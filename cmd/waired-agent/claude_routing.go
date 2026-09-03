package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// claudeRoutingController remembers what the Claude surface was asked for and
// what answered, for the surfaces that cannot see the traffic themselves — the
// statusline, the tray and `waired claude status`.
//
// It used to own a per-class routing policy as well (which of auto / waired /
// anthropic served the main conversation and which served subagents). The
// policy is gone: a turn runs where its model id says, and waired holds no
// route that could send it elsewhere
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). What is left is a record, so nothing
// here steers a request.
//
// It lives for the whole process lifetime so the records survive a session
// restart, and it implements management.ClaudeRoutingControl.
type claudeRoutingController struct {
	logger *slog.Logger

	mu             sync.Mutex // serialises the last* fields
	lastLocalModel string
	lastServedBy   string    // peer DeviceID; "" = this device
	lastServedAt   time.Time // zero until the first waired-served request

	// The turn a request ASKED for, which is a different question from what
	// answered it: a /model pick can send a turn to the real Anthropic API,
	// and such a turn produces no served record at all.
	lastRequestModel string
	lastRequestRoute string
	lastRequestAt    time.Time

	ring *observability.Ring // optional; nil disables emission
}

func newClaudeRoutingController(logger *slog.Logger) *claudeRoutingController {
	if logger == nil {
		logger = slog.Default()
	}
	return &claudeRoutingController{logger: logger}
}

// WithObservability wires the optional event ring. Returns the receiver for
// chaining.
func (c *claudeRoutingController) WithObservability(r *observability.Ring) *claudeRoutingController {
	c.ring = r
	return c
}

// RecordServed is the intercept OnServed hook: it remembers the catalog model
// id that answered the last waired-served Claude request plus the serving peer
// ("" = this device), so the statusline can show which model is doing the work
// and where (#601/#602). Fires per request, so it stays quiet in the logs.
//
// The record is never cleared, so it keeps answering "when did Waired last
// serve a turn, and what answered it". That is only readable alongside the
// time it happened: without one, a stale record reads as if Waired were still
// serving (#755). LastRequestAt carries a timestamp for the same reason.
func (c *claudeRoutingController) RecordServed(modelID, peerDeviceID string) {
	c.mu.Lock()
	c.lastLocalModel = modelID
	c.lastServedBy = peerDeviceID
	c.lastServedAt = time.Now().UTC()
	c.mu.Unlock()
}

// RecordRequest is the intercept OnRequest hook: it remembers the model id the
// last Claude turn carried and the side that id named. RecordServed answers
// "what answered"; this answers "what was asked for". Both are needed on the
// diagnostic surfaces, because a turn the user sent to the real Anthropic API
// by naming a model never reaches RecordServed (waired-agent#1036 asked for
// exactly this line: a session routed somewhere the user did not expect was
// invisible from the host). It is also what the routing sentinel asserts
// against (docs/decisions/20260829/1655-the-sentinel-observes-the-decision.md).
func (c *claudeRoutingController) RecordRequest(model, route, class string) {
	if class == string(state.ClaudeClassSub) {
		// Subagent traffic carries its own pinned label, not the user's pick.
		// Recording it would overwrite the main conversation's model with a
		// string the user never chose.
		return
	}
	c.mu.Lock()
	c.lastRequestModel = model
	c.lastRequestRoute = route
	c.lastRequestAt = time.Now().UTC()
	c.mu.Unlock()
}

// State reports the last-served and last-requested records
// (management.ClaudeRoutingControl).
func (c *claudeRoutingController) State() management.ClaudeRoutingState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return management.ClaudeRoutingState{
		LastLocalModel:   c.lastLocalModel,
		LastServedBy:     c.lastServedBy,
		LastServedAt:     c.lastServedAt,
		LastRequestModel: c.lastRequestModel,
		LastRequestRoute: c.lastRequestRoute,
		LastRequestAt:    c.lastRequestAt,
	}
}
