package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// nodeKeyRotator drives the automatic Node Key 180-day rotation (#228,
// auth spec §13). It is session-scoped: built inside the daemon's
// activate() closure (so it has the session's tokens / machine key /
// current node key) and torn down with the session. On a successful
// rotation it persists the new key + cert + meta and triggers a session
// re-activation, which rebuilds the engine / multiplex-bind / relay
// factory / disco from the rotated key on disk — there is no in-place
// self-key hot-swap, so no capture point can be missed.
type nodeKeyRotator struct {
	cfg    nodeKeyRotatorConfig
	client *controlclient.Client
}

type nodeKeyRotatorConfig struct {
	StateDir   string
	ControlURL string
	DeviceID   string
	NetworkID  string
	MachineKey *devicekeys.MachineKey
	// CurrentNodeKey is the key the session was built with (the rotation
	// "old" key). Loaded from disk in activate().
	CurrentNodeKey *devicekeys.NodeKey

	HTTPClient          *http.Client // nil → controlclient default
	BearerFn            func() string
	UseCustomAuthHeader bool

	// TriggerReactivate rebuilds the session from the rotated key on disk.
	// MUST be non-blocking / detached: it tears down THIS session, which
	// cancels the rotator's own context, so it cannot run on the rotator
	// goroutine. The daemon wires it as `func() { go reactivate() }`.
	TriggerReactivate func()

	Logger *slog.Logger
	// Now and CheckInterval are injectable for tests.
	Now           func() time.Time
	CheckInterval time.Duration
}

func newNodeKeyRotator(cfg nodeKeyRotatorConfig) *nodeKeyRotator {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = 6 * time.Hour
	}
	client := controlclient.NewWithBearer(cfg.ControlURL, cfg.BearerFn)
	if cfg.HTTPClient != nil {
		client.HTTP = cfg.HTTPClient
	}
	client.UseCustomAuthHeader = cfg.UseCustomAuthHeader
	return &nodeKeyRotator{cfg: cfg, client: client}
}

func (r *nodeKeyRotator) now() time.Time {
	if r.cfg.Now != nil {
		return r.cfg.Now()
	}
	return time.Now()
}

// Run is the rotation loop. It first reconciles any interrupted prior
// rotation (a staged node.key.next), then sleeps until the scheduled
// rotation time, re-checking on CheckInterval so a lifetime change or a
// late-populated meta is picked up. It returns when ctx is cancelled
// (session teardown) or after it has triggered a re-activation.
func (r *nodeKeyRotator) Run(ctx context.Context) {
	if r.recoverStagedRotation(ctx) {
		// A staged rotation was completed; we've triggered a re-activation
		// and this session is about to be torn down. Stop.
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.nextSleep()):
		}
		done, err := r.maybeRotate(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.cfg.Logger.Warn("node-key rotation attempt failed; will retry", "err", err)
			continue
		}
		if done {
			// Rotation succeeded and a re-activation is in flight.
			return
		}
	}
}

func (r *nodeKeyRotator) nextSleep() time.Duration {
	meta, err := identity.LoadNodeKeyMeta(r.cfg.StateDir)
	if err != nil || meta.ExpiresAt.IsZero() {
		return r.cfg.CheckInterval
	}
	d := meta.ExpiresAt.Add(-signer.NodeKeyRotationLead).Sub(r.now())
	switch {
	case d < 0:
		return 0
	case d > r.cfg.CheckInterval:
		return r.cfg.CheckInterval
	default:
		return d
	}
}

// maybeRotate rotates when the scheduled time has arrived. Returns
// (true, nil) when a rotation succeeded and a re-activation was
// triggered; (false, nil) when it is not yet time.
func (r *nodeKeyRotator) maybeRotate(ctx context.Context) (bool, error) {
	meta, err := identity.LoadNodeKeyMeta(r.cfg.StateDir)
	if err != nil {
		return false, err
	}
	if meta.ExpiresAt.IsZero() {
		return false, nil // no schedule yet; wait for a refresh to populate it
	}
	if r.now().Before(meta.ExpiresAt.Add(-signer.NodeKeyRotationLead)) {
		return false, nil // not yet
	}
	newKey, err := devicekeys.NewNodeKey()
	if err != nil {
		return false, err
	}
	return r.rotateTo(ctx, newKey)
}

// rotateTo stages newKey, registers it with the CP, promotes it, persists
// the new cert + meta, and triggers a re-activation. The staging-before-
// POST ordering is crash-safe: recoverStagedRotation completes or discards
// a node.key.next left behind by an interrupted attempt.
func (r *nodeKeyRotator) rotateTo(ctx context.Context, newKey *devicekeys.NodeKey) (bool, error) {
	p, err := identity.PathsFor(r.cfg.StateDir)
	if err != nil {
		return false, err
	}
	if err := devicekeys.SaveNodeKey(p.NodeKeyNext, newKey); err != nil {
		return false, err
	}
	res, err := r.client.RotateNodeKey(ctx, controlclient.RotateNodeKeyParams{
		DeviceID:         r.cfg.DeviceID,
		NetworkID:        r.cfg.NetworkID,
		OldNodePublicKey: r.cfg.CurrentNodeKey.PublicBase64(),
		NewNodePublicKey: newKey.PublicBase64(),
		MachineKey:       r.cfg.MachineKey,
	})
	if err != nil {
		// Leave the staged key in place only for a mismatch (the CP may
		// already hold it — see recoverStagedRotation); otherwise discard
		// so a transient failure retries cleanly with a fresh key.
		if !errors.Is(err, controlclient.ErrNodeKeyMismatch) {
			_ = os.Remove(p.NodeKeyNext)
		}
		return false, err
	}
	return true, r.applyRotated(p, res.DeviceCertificateJSON, res.NodeKeyExpiresAt)
}

// applyRotated promotes the staged key, persists the new cert + meta, and
// triggers the re-activation. Promote happens before the cert/meta writes
// so the only window where the CP holds the new key but disk holds the old
// one is the rename syscall itself.
func (r *nodeKeyRotator) applyRotated(p *identity.Paths, certJSON []byte, expiresAt time.Time) error {
	if err := os.Rename(p.NodeKeyNext, p.NodeKey); err != nil {
		return err
	}
	if len(certJSON) > 0 {
		if err := identity.SaveBytes(p.DeviceCertificate, certJSON, 0o644); err != nil {
			r.cfg.Logger.Warn("persist rotated device certificate", "err", err)
		}
	}
	if err := identity.SaveNodeKeyMeta(r.cfg.StateDir, identity.NodeKeyMeta{IssuedAt: r.now(), ExpiresAt: expiresAt}); err != nil {
		r.cfg.Logger.Warn("persist node-key meta", "err", err)
	}
	r.cfg.Logger.Info("node key rotated; re-activating session to apply new key",
		"device_id", r.cfg.DeviceID, "node_key_expires_at", expiresAt)
	if r.cfg.TriggerReactivate != nil {
		r.cfg.TriggerReactivate()
	}
	return nil
}

// recoverStagedRotation completes or discards a node.key.next left behind
// by an interrupted rotation (#228 crash-safety). It returns true when it
// completed a rotation (and triggered a re-activation), so Run should stop.
//
//   - No staged key: nothing to do.
//   - Staged key + CP accepts (current key still matches): the prior POST
//     never committed; finish the rotation now.
//   - Staged key + ErrNodeKeyMismatch: the CP's current key is no longer
//     our on-disk key. In the single-agent-per-device model that means the
//     CP already committed this very staged key (the prior POST succeeded
//     but the promote didn't), so we adopt it.
//   - Other error: leave the staged key for the next attempt.
func (r *nodeKeyRotator) recoverStagedRotation(ctx context.Context) bool {
	p, err := identity.PathsFor(r.cfg.StateDir)
	if err != nil {
		return false
	}
	staged, err := loadStagedNodeKey(p.NodeKeyNext)
	if err != nil || staged == nil {
		return false
	}
	r.cfg.Logger.Warn("interrupted node-key rotation detected; reconciling", "staged_key", staged.PublicBase64())
	res, rerr := r.client.RotateNodeKey(ctx, controlclient.RotateNodeKeyParams{
		DeviceID:         r.cfg.DeviceID,
		NetworkID:        r.cfg.NetworkID,
		OldNodePublicKey: r.cfg.CurrentNodeKey.PublicBase64(),
		NewNodePublicKey: staged.PublicBase64(),
		MachineKey:       r.cfg.MachineKey,
	})
	switch {
	case rerr == nil:
		if err := r.applyRotated(p, res.DeviceCertificateJSON, res.NodeKeyExpiresAt); err != nil {
			r.cfg.Logger.Warn("apply recovered rotation", "err", err)
			return false
		}
		return true
	case errors.Is(rerr, controlclient.ErrNodeKeyMismatch):
		// CP already holds the staged key; adopt it locally and re-activate.
		r.cfg.Logger.Info("adopting staged node key already committed at CP")
		if err := os.Rename(p.NodeKeyNext, p.NodeKey); err != nil {
			r.cfg.Logger.Warn("adopt staged node key", "err", err)
			return false
		}
		if r.cfg.TriggerReactivate != nil {
			r.cfg.TriggerReactivate()
		}
		return true
	default:
		r.cfg.Logger.Warn("could not reconcile interrupted rotation; will retry", "err", rerr)
		return false
	}
}

// loadStagedNodeKey reads the staged node.key.next, returning (nil, nil)
// when it does not exist.
func loadStagedNodeKey(path string) (*devicekeys.NodeKey, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return devicekeys.LoadOrCreateNodeKey(path)
}
