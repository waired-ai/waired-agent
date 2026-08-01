package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
)

// activationRetryMin / activationRetryMax bound the backoff between boot
// activation attempts (waired-agent#318). Activation used to be a single
// shot: one failure and the daemon stayed unenrolled until something
// restarted it, so a transient condition at boot — the WireGuard port
// landing inside a Windows excluded UDP range, the network not up yet —
// cost the user the whole session.
const (
	activationRetryMin = 15 * time.Second
	activationRetryMax = 5 * time.Minute
)

// nextActivationBackoff doubles d, starting from activationRetryMin and
// capped at activationRetryMax.
func nextActivationBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return activationRetryMin
	}
	return min(d*2, activationRetryMax)
}

// runBootActivation brings the identity-dependent runtime up, retrying
// on failure until it succeeds or the daemon shuts down.
//
// Every attempt's error is recorded on the switchboard so the management
// API — and through it the tray and `waired doctor` — can say what is
// wrong while the device sits inactive, instead of presenting a
// signed-in host as signed out.
//
// A login completing in the meantime publishes its own session; the
// activate closure refuses to publish a second one, so this loop stops
// as soon as any session is live.
func runBootActivation(ctx context.Context, sb *switchboard, activate func(context.Context) error, logger *slog.Logger) {
	var backoff time.Duration
	for {
		if sb.current() != nil {
			return
		}
		err := activate(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		sb.noteActivationError(err)
		backoff = nextActivationBackoff(backoff)
		logger.Error("activate session at boot; will retry",
			"err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// offlineIdentityView projects a persisted identity into the view the
// management API serves while no session is live. Active is false by
// construction — that is the whole point of the offline view.
func offlineIdentityView(id *identity.Identity) management.IdentityView {
	deviceName := id.DeviceName
	if deviceName == "" {
		deviceName = id.DeviceID
	}
	return management.IdentityView{
		Enrolled:     true,
		AccountEmail: id.AccountEmail,
		NetworkName:  id.NetworkName,
		NetworkID:    id.NetworkID,
		DeviceID:     id.DeviceID,
		DeviceName:   deviceName,
		OverlayIP:    id.OverlayIP,
		ControlURL:   id.ControlURL,
	}
}

// boundEngine is the slice of the WireGuard engine that bringUpEngine
// needs: the port it actually bound. Kept as a constraint rather than a
// concrete type so the fallback logic can be exercised without opening a
// real socket — a bind failure on a privileged or OS-excluded port is
// not something a unit test can arrange portably.
type boundEngine interface {
	ListenPort() (int, error)
}

// bringUpEngine starts the WireGuard engine, falling back to an
// ephemeral port when the preferred one cannot be bound.
//
// The persisted endpoint pins a UDP port chosen at enrollment. On
// Windows that port can become unbindable between boots without anything
// changing on our side: winnat/Hyper-V reserve UDP ranges that are
// reshuffled on every boot, and a bind inside one fails with a
// permissions error. Treating that as fatal left a signed-in device
// inactive for the entire session (waired-agent#318). Any free port
// works instead — peers learn the real one from the endpoint candidates
// this agent advertises, not from the enrollment record — so retry once
// on port 0 and carry on.
//
// Returns the engine and the port it is really listening on.
func bringUpEngine[E boundEngine](cfg wgnet.Config, newEngine func(wgnet.Config) (E, error), logger *slog.Logger) (E, int, error) {
	var zero E
	engine, err := newEngine(cfg)
	if err == nil {
		return engine, boundPort(engine, cfg.ListenPort), nil
	}
	// Only a bind failure is retryable, and only when we asked for a
	// specific port — a failure on port 0 means no port was available at
	// all, which another attempt will not fix.
	if !errors.Is(err, wgnet.ErrBindFailed) || cfg.ListenPort == 0 {
		return zero, 0, err
	}
	logger.Warn("could not bind the preferred UDP port; retrying on any free port",
		"preferred_port", cfg.ListenPort, "err", err)

	ephemeral := cfg
	ephemeral.ListenPort = 0
	engine, err = newEngine(ephemeral)
	if err != nil {
		return zero, 0, err
	}
	port := boundPort(engine, 0)
	logger.Warn("WireGuard is listening on a fallback port for this session",
		"preferred_port", cfg.ListenPort, "listen_port", port)
	return engine, port, nil
}

// boundPort asks the engine which port it bound, falling back to the
// requested one when the device cannot say. A wrong-but-plausible port
// here only degrades the endpoint candidates we advertise; it must not
// fail activation.
func boundPort(e boundEngine, requested int) int {
	if port, err := e.ListenPort(); err == nil && port != 0 {
		return port
	}
	return requested
}
