package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// connectivityPushDeps bundles the collaborators the connectivity-push
// loop needs (#252). It is intentionally tiny: the loop only READS the
// reconciler's already-maintained per-peer path state via Snapshot and
// forwards a summary — it does not touch path selection.
type connectivityPushDeps struct {
	PushClient *controlclient.Client // nil = no CP push (loop is a no-op)
	DeviceID   string
	MachineKey ed25519.PrivateKey
	// Snapshot returns the reconciler's per-peer path state, keyed by
	// node public key. Typically rec.Snapshot.
	Snapshot func() map[string]PathSnapshot
	Logger   *slog.Logger
}

// runConnectivityPush periodically reports the device's direct/relay
// connectivity summary to the Control Plane (#252) for display on the
// admin Device detail page. It mirrors runLocalInferenceProbe's cadence
// (state.HeartbeatInterval) and the CP's per-device rate limit, and is
// purely additive: it observes reconciler state and never mutates it.
func runConnectivityPush(ctx context.Context, deps connectivityPushDeps) {
	if deps.PushClient == nil || deps.DeviceID == "" ||
		len(deps.MachineKey) != ed25519.PrivateKeySize || deps.Snapshot == nil {
		return
	}

	tick := func() {
		cs := summarizeConnectivity(deps.Snapshot())
		pushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := deps.PushClient.PushConnectivityStatus(pushCtx, deps.DeviceID, cs, deps.MachineKey)
		cancel()
		if err != nil && deps.Logger != nil && !errors.Is(err, context.Canceled) {
			deps.Logger.Warn("connectivity status push failed", "err", err)
		}
	}

	tick()

	t := time.NewTicker(state.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// summarizeConnectivity reduces the reconciler's per-peer path snapshot
// to the aggregate counts the CP stores. A peer whose CurrentPath is
// neither direct nor relay (e.g. not yet established) is counted in
// TotalPeers only, so direct+relay never exceeds total — the same
// invariant the CP validator enforces. Pure: no I/O, safe to unit-test.
func summarizeConnectivity(snap map[string]PathSnapshot) signer.ConnectivityState {
	var direct, relay int
	for _, ps := range snap {
		switch ps.CurrentPath {
		case pathDirect:
			direct++
		case pathRelay:
			relay++
		}
	}
	return signer.ConnectivityState{
		DirectPeers: direct,
		RelayPeers:  relay,
		TotalPeers:  len(snap),
		LastCheck:   time.Now().UTC().Format(time.RFC3339Nano),
	}
}
