package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
)

// tokenRefresher owns the live access / refresh token pair plus the
// expiry metadata. Other agent goroutines pull the current access
// token via Get() (passed as a `func() string` into every
// controlclient.NewWithBearer / runFooLoop). Run() drives the
// auto-refresh schedule.
//
// Concurrency: Get() is hot-path (every authenticated CP request) and
// must be lock-free. We back it with an atomic.Pointer[string].
// SetAccessToken / SetRefreshToken / setMeta are only touched by Run()
// and a single waired-auth-renew CLI path, so a Mutex around the
// refresh-token + meta is sufficient.
type tokenRefresher struct {
	stateDir   string
	controlURL string
	deviceID   string
	networkID  string

	machineKey *devicekeys.MachineKey
	httpClient *http.Client

	accessToken atomic.Pointer[string]

	// Read by Run() to know whether refresh is even possible. Written
	// at construction and after each successful refresh.
	refreshToken atomic.Pointer[string]

	// expires is the cached access-token expiry; Run() schedules the
	// next refresh against expires - refreshLead. Updated atomically
	// via swapMeta.
	expires atomic.Pointer[tokenMeta]

	// refreshLead is how long before access-token expiry we kick off a
	// refresh. Default: 2 minutes.
	refreshLead time.Duration

	// minSleep guards against a misconfigured / stale expiry that
	// would make the loop spin. Default: 30 seconds.
	minSleep time.Duration

	logger *slog.Logger
}

type tokenMeta struct {
	accessExpiresAt     time.Time
	deviceAuthExpiresAt time.Time
}

type tokenRefresherConfig struct {
	StateDir          string
	ControlURL        string
	DeviceID          string
	NetworkID         string
	MachineKey        *devicekeys.MachineKey
	HTTPClient        *http.Client
	InitialAccess     string
	InitialRefresh    string
	InitialMeta       identity.TokenMeta
	RefreshLead       time.Duration
	MinSleepOnFailure time.Duration
	Logger            *slog.Logger
}

func newTokenRefresher(cfg tokenRefresherConfig) *tokenRefresher {
	if cfg.RefreshLead == 0 {
		cfg.RefreshLead = 2 * time.Minute
	}
	if cfg.MinSleepOnFailure == 0 {
		cfg.MinSleepOnFailure = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &tokenRefresher{
		stateDir:    cfg.StateDir,
		controlURL:  cfg.ControlURL,
		deviceID:    cfg.DeviceID,
		networkID:   cfg.NetworkID,
		machineKey:  cfg.MachineKey,
		httpClient:  cfg.HTTPClient,
		refreshLead: cfg.RefreshLead,
		minSleep:    cfg.MinSleepOnFailure,
		logger:      cfg.Logger.With("component", "token-refresher"),
	}
	access := cfg.InitialAccess
	r.accessToken.Store(&access)
	refresh := cfg.InitialRefresh
	r.refreshToken.Store(&refresh)
	r.expires.Store(&tokenMeta{
		accessExpiresAt:     cfg.InitialMeta.AccessExpiresAt,
		deviceAuthExpiresAt: cfg.InitialMeta.DeviceAuthExpiresAt,
	})
	return r
}

// Get returns the current access token. Hot path; lock-free.
func (r *tokenRefresher) Get() string {
	if p := r.accessToken.Load(); p != nil {
		return *p
	}
	return ""
}

// Run drives the refresh schedule until ctx is cancelled. Exits
// silently on context cancel; logs everything else via r.logger.
func (r *tokenRefresher) Run(ctx context.Context) {
	for {
		if !r.canRefresh() {
			r.logger.Info("no refresh token persisted; auto-refresh disabled (run `waired init` to enroll or re-authenticate)")
			return
		}
		sleep := r.nextSleep(time.Now())
		r.logger.Debug("token refresh scheduled", "sleep", sleep.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		if err := r.refreshOnce(ctx); err != nil {
			r.logger.Warn("refresh failed; backing off",
				"err", err, "backoff", r.minSleep)
			if errors.Is(err, controlclient.ErrRefreshReuseDetected) ||
				errors.Is(err, controlclient.ErrRefreshInvalid) ||
				errors.Is(err, controlclient.ErrRefreshExpired) ||
				errors.Is(err, controlclient.ErrReauthRequired) ||
				errors.Is(err, controlclient.ErrDeviceNotApproved) {
				// Terminal: refresh will never succeed again until
				// the user re-OAuths. Persist the state so
				// `waired auth status` (and any future tray /
				// web-admin surface) can tell the operator
				// *something* is wrong even when the daemon
				// hasn't been restarted. Then stop the loop so we
				// don't spin pointless requests at the CP.
				r.markReauthRequired(err)
				return
			}
			if errors.Is(err, controlclient.ErrDeviceSuspended) {
				// Reversible pause (#248): intentionally NOT in the
				// terminal set above. Fall through to the backoff+retry
				// below so the device comes back on its own once the
				// owner re-enables it — no `waired init` required.
				r.logger.Info("device suspended; refresh will retry until re-enabled")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.minSleep):
			}
		}
	}
}

// markReauthRequired persists the "user must re-OAuth" flag to
// cache/token_meta.json so out-of-band callers (`waired auth status`,
// future tray badge) can surface the state even before the daemon
// restarts. Best-effort: failure to write is logged but does not block
// the refresh-loop exit because the in-memory state is already
// "we gave up".
func (r *tokenRefresher) markReauthRequired(cause error) {
	meta := identity.TokenMeta{
		ReauthRequiredAt: time.Now().UTC(),
	}
	if m := r.expires.Load(); m != nil {
		meta.AccessExpiresAt = m.accessExpiresAt
		meta.DeviceAuthExpiresAt = m.deviceAuthExpiresAt
	}
	if err := identity.SaveTokenMeta(r.stateDir, meta); err != nil {
		r.logger.Error("persist reauth_required flag failed",
			"err", err, "cause", cause)
		return
	}
	r.logger.Warn("device flagged reauth_required; run `waired init` to recover",
		"cause", cause)
}

func (r *tokenRefresher) canRefresh() bool {
	if r.machineKey == nil {
		return false
	}
	if p := r.refreshToken.Load(); p == nil || *p == "" {
		return false
	}
	if r.deviceID == "" || r.networkID == "" || r.controlURL == "" {
		return false
	}
	return true
}

func (r *tokenRefresher) nextSleep(now time.Time) time.Duration {
	m := r.expires.Load()
	if m == nil || m.accessExpiresAt.IsZero() {
		// No expiry info on disk (pre-Phase-B agent state). Refresh
		// soon to populate it.
		return r.minSleep
	}
	target := m.accessExpiresAt.Add(-r.refreshLead)
	d := target.Sub(now)
	if d < r.minSleep {
		return r.minSleep
	}
	return d
}

func (r *tokenRefresher) refreshOnce(ctx context.Context) error {
	r.logger.Debug("token refresh attempt", "device_id", r.deviceID, "network_id", r.networkID)
	refresh := ""
	if p := r.refreshToken.Load(); p != nil {
		refresh = *p
	}
	res, err := controlclient.RefreshDeviceToken(ctx, controlclient.RefreshParams{
		ControlURL:   r.controlURL,
		DeviceID:     r.deviceID,
		NetworkID:    r.networkID,
		RefreshToken: refresh,
		MachineKey:   r.machineKey,
		HTTPClient:   r.httpClient,
	})
	if err != nil {
		return err
	}

	// Persist before publishing in-memory so a crash mid-rotation
	// doesn't leave the new token live in RAM but lost on disk.
	if err := identity.SaveAccessToken(r.stateDir, res.DeviceAccessToken); err != nil {
		return err
	}
	if err := identity.SaveRefreshToken(r.stateDir, res.DeviceRefreshToken); err != nil {
		return err
	}
	if err := identity.SaveTokenMeta(r.stateDir, identity.TokenMeta{
		AccessExpiresAt:     res.DeviceAccessTokenExpiresAt,
		DeviceAuthExpiresAt: res.DeviceAuthExpiresAt,
	}); err != nil {
		return err
	}
	if len(res.DeviceCertificateJSON) > 0 {
		// Best-effort: stash the fresh cert too. The agent's running
		// network-map subscription will eventually re-fetch via the
		// signed map, but persisting it here keeps disk state
		// consistent for next agent start.
		paths, err := identity.PathsFor(r.stateDir)
		if err == nil {
			_ = identity.SaveBytes(paths.DeviceCertificate, res.DeviceCertificateJSON, 0o644)
		}
	}
	// Keep the Node Key rotation clock fresh (#228): the refresh response
	// carries node_key_expires_at so the rotation loop has an authoritative
	// schedule even on a long-running agent that never restarts. Preserve
	// the existing IssuedAt. Zero from a pre-#228 CP → leave meta as-is.
	if !res.NodeKeyExpiresAt.IsZero() {
		nkMeta, _ := identity.LoadNodeKeyMeta(r.stateDir)
		nkMeta.ExpiresAt = res.NodeKeyExpiresAt
		_ = identity.SaveNodeKeyMeta(r.stateDir, nkMeta)
	}

	access := res.DeviceAccessToken
	newRefresh := res.DeviceRefreshToken
	r.accessToken.Store(&access)
	r.refreshToken.Store(&newRefresh)
	r.expires.Store(&tokenMeta{
		accessExpiresAt:     res.DeviceAccessTokenExpiresAt,
		deviceAuthExpiresAt: res.DeviceAuthExpiresAt,
	})
	r.logger.Info("access token refreshed",
		"access_expires_at", res.DeviceAccessTokenExpiresAt.UTC().Format(time.RFC3339),
		"device_auth_expires_at", res.DeviceAuthExpiresAt.UTC().Format(time.RFC3339),
	)
	return nil
}
