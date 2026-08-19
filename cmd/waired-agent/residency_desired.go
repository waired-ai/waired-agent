package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The control plane's half of the model-residency setting
// (waired-agent#861): NAVI writes how long this device's engine should
// keep a model in memory after the last request, the value rides the
// signed map's Self entry as InferenceState.DesiredIdleTimeout, and this
// applies it through the SAME controller the CLI and the app use.
//
// It is applied from applySelf — "the CP's effective per-device
// settings" — and deliberately NOT folded into setupDesired. Residency
// applies to every enrolled device, whereas setupDesired's zero value is
// what tells the reconciler nobody is driving a wizard; a device whose
// only instruction was a residency setting would otherwise be reported
// as one in the middle of setup.

// desiredResidency applies control-plane residency instructions once per
// distinct value.
//
// Once per VALUE, not once per frame, and the distinction is the whole
// design. The control plane re-sends its instruction on every map frame,
// so a reconciler that applied it each time would revert a local change
// from `waired inference residency` or the app within the poll interval
// — making both of those controls a lie. Last writer wins; the control
// plane wins only when its value actually changes.
type desiredResidency struct {
	ctl      residencySetter
	stateDir string
	logger   *slog.Logger
	now      func() time.Time

	mu    sync.Mutex
	acted state.AppliedResidency
}

// residencySetter is the half of residencyController this needs. Named
// separately so the apply logic is testable without agent.json and a
// live engine behind it.
type residencySetter interface {
	SetResidency(ctx context.Context, idle time.Duration) (time.Duration, error)
}

// newDesiredResidency seeds the acted-on marker from disk, so a daemon
// restart does not re-apply an instruction it already honoured over a
// local change made since.
func newDesiredResidency(ctl residencySetter, stateDir string, logger *slog.Logger) *desiredResidency {
	d := &desiredResidency{ctl: ctl, stateDir: stateDir, logger: logger, now: time.Now}
	if stateDir == "" {
		return d
	}
	rec, err := state.ReadAppliedResidency(stateDir)
	if err != nil {
		// An unreadable record is treated as no record. The cost is one
		// re-application of the current instruction, which is the safe
		// direction: the alternative is never applying a new one.
		if logger != nil {
			logger.Warn("could not read the applied-residency record; a control-plane instruction may be re-applied",
				"err", err)
		}
		return d
	}
	d.acted = rec
	return d
}

// Apply acts on one map frame's instruction.
//
// An empty value is no instruction at all — the device keeps whatever it
// has, which is what lets clearing the value in the control plane hand
// the device back to local control rather than pinning it to a default.
//
// A value this build cannot parse is left un-acted AND unrecorded, so a
// newer control plane speaking a vocabulary this agent does not know
// finds its instruction still pending after an upgrade rather than
// consumed — the treatment applyDesiredInference gives an unknown value.
func (d *desiredResidency) Apply(ctx context.Context, value string) {
	if d == nil || value == "" {
		return
	}
	idle, err := management.ParseResidency(value)
	if err != nil {
		if d.logger != nil {
			d.logger.Warn("control plane asked for a model residency this build cannot read; leaving it pending",
				"desired_idle_timeout", value)
		}
		return
	}
	// Compare and record the CANONICAL form, not the wire string: "0",
	// "0s" and "-1h" all mean the same instruction, and keying the marker
	// on the spelling would re-apply the same ask every time the control
	// plane changed how it writes it.
	canonical := idle.String()

	d.mu.Lock()
	if d.acted.Value == canonical {
		d.mu.Unlock()
		return
	}
	rec := state.AppliedResidency{Value: canonical, AppliedAt: d.now().UTC().Format(time.RFC3339)}
	d.acted = rec
	d.mu.Unlock()

	if _, err := d.ctl.SetResidency(ctx, idle); err != nil && d.logger != nil {
		// The controller reports a live failure and a persistence failure
		// distinctly and applies what it can, so this is a warning rather
		// than a reason to un-record: re-applying on the next frame would
		// fight a local change for as long as the engine stayed unwell.
		d.logger.Warn("could not fully apply the control plane's model-residency instruction",
			"desired_idle_timeout", canonical, "err", err)
	} else if d.logger != nil {
		d.logger.Info("applied the control plane's model-residency instruction",
			"idle_timeout", canonical, "holds_indefinitely", idle == 0)
	}

	if d.stateDir == "" {
		return
	}
	if err := state.WriteAppliedResidency(d.stateDir, rec); err != nil && d.logger != nil {
		d.logger.Warn("could not persist the applied-residency record; a restart may re-apply the instruction",
			"value", canonical, "err", err)
	}
}

// residencyCapabilities is the capability this agent declares for the
// field above. Kept beside the reader so the declaration and the
// consumer cannot drift apart: declaring it without reading the field
// would have the control plane emit an instruction nothing acts on.
var residencyCapabilities = []string{signer.CapabilityResidencyV1}
