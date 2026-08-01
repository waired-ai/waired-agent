package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

	// pending remembers a rotation whose verdict never arrived, so the
	// retry can present the byte-identical request instead of a fresh
	// one. Only Run()'s goroutine touches it (refreshOnce is not called
	// concurrently), so a plain field is enough.
	pending *pendingRotation

	// replayWindow is how long a pending rotation stays replayable. Must
	// stay inside the CP's replay grace — past it the CP can no longer
	// tell the retry from a replay, so we start a fresh rotation instead
	// of knowingly tripping reuse detection. Default: 60 seconds.
	replayWindow time.Duration

	// retryDelay is the pause before the single in-flow retry of a
	// rotation whose response was lost. Short on purpose: the retry has
	// to land well inside replayWindow. Default: 2 seconds.
	retryDelay time.Duration

	// onTerminal, when set, fires once with the classified cause just
	// before Run() gives up for good. The agent uses it to quiesce its
	// Control-Plane push loops (an access token that will never be
	// renewed 401s forever otherwise) and to surface the state to the
	// tray / doctor.
	onTerminal func(error)

	logger *slog.Logger
}

// pendingRotation is a rotation attempt whose outcome we never learned.
// refreshToken pins it to the credential it was made with: if the token
// changed underneath (a later attempt succeeded), the pending nonce is
// stale and must not be replayed.
type pendingRotation struct {
	nonce        string
	refreshToken string
	at           time.Time
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
	ReplayWindow      time.Duration
	RetryDelay        time.Duration
	// OnTerminal fires once when auto-refresh gives up for good. See
	// tokenRefresher.onTerminal.
	OnTerminal func(error)
	Logger     *slog.Logger
}

func newTokenRefresher(cfg tokenRefresherConfig) *tokenRefresher {
	if cfg.RefreshLead == 0 {
		cfg.RefreshLead = 2 * time.Minute
	}
	if cfg.MinSleepOnFailure == 0 {
		cfg.MinSleepOnFailure = 30 * time.Second
	}
	if cfg.ReplayWindow == 0 {
		cfg.ReplayWindow = 60 * time.Second
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &tokenRefresher{
		stateDir:     cfg.StateDir,
		controlURL:   cfg.ControlURL,
		deviceID:     cfg.DeviceID,
		networkID:    cfg.NetworkID,
		machineKey:   cfg.MachineKey,
		httpClient:   cfg.HTTPClient,
		refreshLead:  cfg.RefreshLead,
		minSleep:     cfg.MinSleepOnFailure,
		replayWindow: cfg.ReplayWindow,
		retryDelay:   cfg.RetryDelay,
		onTerminal:   cfg.OnTerminal,
		logger:       cfg.Logger.With("component", "token-refresher"),
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
			if isTerminalRefreshError(err) {
				// Terminal: refresh will never succeed again until
				// the user re-OAuths. Persist the state so
				// `waired auth status` (and any future tray /
				// web-admin surface) can tell the operator
				// *something* is wrong even when the daemon
				// hasn't been restarted. Then stop the loop so we
				// don't spin pointless requests at the CP.
				r.markReauthRequired(err)
				// Quiesce the CP-facing loops (waired-agent#318): they
				// hold a bearer that will never be renewed, and left
				// running they 401-storm the Control Plane for the rest
				// of the process's life.
				if r.onTerminal != nil {
					r.onTerminal(err)
				}
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
			continue
		}
		// Anti-spin: nextSleep now returns 0 whenever the refresh point
		// has passed, so a CP that answers 200 with an already-stale
		// expiry would otherwise put this loop in a hot cycle.
		if r.nextSleep(time.Now()) == 0 {
			r.logger.Warn("refreshed access token is already past its refresh point; backing off",
				"backoff", r.minSleep)
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.minSleep):
			}
		}
	}
}

// isTerminalRefreshError reports whether auto-refresh can never succeed
// again without the user re-OAuthing via `waired init`.
//
// ErrDeviceSuspended is deliberately absent: it is a reversible pause
// (#248) that recovers on its own once the owner re-enables the device.
// So is ErrRefreshOutcomeUnknown — a lost verdict says nothing about
// whether the credential still works.
func isTerminalRefreshError(err error) bool {
	return errors.Is(err, controlclient.ErrRefreshReuseDetected) ||
		errors.Is(err, controlclient.ErrRefreshInvalid) ||
		errors.Is(err, controlclient.ErrRefreshExpired) ||
		errors.Is(err, controlclient.ErrReauthRequired) ||
		errors.Is(err, controlclient.ErrDeviceNotApproved)
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

// nextSleep is how long to wait before the next rotation attempt.
//
// Contract (waired-agent#318): when the refresh point has already passed
// — the access token is expired, inside the lead, or its expiry is
// unknown — this returns 0, so the very first thing a freshly started
// daemon does is renew. It used to floor at minSleep, which is exactly
// the ~30s of 401s a cold-booted host sprayed at the Control Plane while
// spending a token that had expired hours earlier. minSleep survives as
// the *failure* backoff (applied by Run) and as the anti-spin floor for
// a CP that hands back an already-stale expiry.
func (r *tokenRefresher) nextSleep(now time.Time) time.Duration {
	m := r.expires.Load()
	if m == nil || m.accessExpiresAt.IsZero() {
		// No expiry info on disk (pre-Phase-B agent state). Refresh
		// immediately to populate it.
		return 0
	}
	target := m.accessExpiresAt.Add(-r.refreshLead)
	if d := target.Sub(now); d > 0 {
		return d
	}
	return 0
}

// accessTokenStale reports whether the cached access token is unusable
// or about to be: no token at all, no recorded expiry, or already inside
// the refresh lead. Callers use it to decide whether to renew *before*
// handing the bearer to the Control-Plane push loops.
func (r *tokenRefresher) accessTokenStale(now time.Time) bool {
	if r.Get() == "" {
		return true
	}
	return r.nextSleep(now) == 0
}

// rotationNonce decides whether this attempt continues a rotation whose
// verdict was lost, or starts a new one.
//
// The pending nonce is replayed only while both hold: the stored refresh
// token is still the one the pending attempt presented (a later success
// would have rotated it), and the attempt is inside replayWindow. Past
// the window the CP can no longer distinguish the replay from a genuine
// reuse, so replaying would knowingly brick the device — start fresh
// instead and take the ordinary classification.
//
// The nonce is minted here rather than inside controlclient because a
// nonce we never saw cannot be replayed.
func (r *tokenRefresher) rotationNonce(refreshToken string, now time.Time) (nonce string, replayed bool) {
	if p := r.pending; p != nil {
		if p.refreshToken == refreshToken && now.Sub(p.at) < r.replayWindow {
			return p.nonce, true
		}
		r.pending = nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not survivable for a signed request;
		// hand back empty and let controlclient surface the error.
		return "", false
	}
	return base64.StdEncoding.EncodeToString(b), false
}

// refreshOnce performs one logical rotation, retrying once when the
// Control Plane's verdict is lost in transit.
//
// Why the retry lives here and not in Run's backoff (waired-agent#318):
// refresh-token rotation is one-time-use, so a response that never
// arrives leaves the agent holding a credential the CP may already have
// burned. Presenting it again is unavoidable — it is the only credential
// we have — but presenting it as a *new* rotation (fresh nonce, minutes
// later) is indistinguishable from theft, and the CP answers by flipping
// the device to reauth_required, which only `waired init` clears. So the
// retry replays the byte-identical request, promptly, inside the window
// where the CP can still match it to the rotation whose reply it lost.
//
// Against a CP without that matching this is a no-op: the replay is
// classified exactly as today's blind retry would be.
func (r *tokenRefresher) refreshOnce(ctx context.Context) error {
	err := r.rotate(ctx, time.Now())
	if !errors.Is(err, controlclient.ErrRefreshOutcomeUnknown) {
		return err
	}
	r.logger.Warn("rotation outcome unknown; replaying the same request",
		"err", err, "retry_in", r.retryDelay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.retryDelay):
	}
	return r.rotate(ctx, time.Now())
}

// rotate is a single POST to the refresh endpoint plus the persistence
// that follows a success. It owns the pending-rotation bookkeeping:
// carrying the nonce forward while the outcome is unknown, and dropping
// it the moment any definite verdict arrives.
func (r *tokenRefresher) rotate(ctx context.Context, now time.Time) error {
	r.logger.Debug("token refresh attempt", "device_id", r.deviceID, "network_id", r.networkID)
	refresh := ""
	if p := r.refreshToken.Load(); p != nil {
		refresh = *p
	}
	nonce, replayed := r.rotationNonce(refresh, now)
	if replayed {
		r.logger.Info("replaying the rotation whose response was lost",
			"age", now.Sub(r.pending.at).Round(time.Second).String())
	}
	res, err := controlclient.RefreshDeviceToken(ctx, controlclient.RefreshParams{
		ControlURL:   r.controlURL,
		DeviceID:     r.deviceID,
		NetworkID:    r.networkID,
		RefreshToken: refresh,
		MachineKey:   r.machineKey,
		HTTPClient:   r.httpClient,
		ClientNonce:  nonce,
	})
	if err != nil {
		if errors.Is(err, controlclient.ErrRefreshOutcomeUnknown) {
			// Keep the original attempt's timestamp: the replay window
			// is measured from when the CP may have committed, not from
			// the latest retry. An empty nonce means controlclient
			// generated its own, which we cannot reproduce — nothing to
			// remember.
			if !replayed && nonce != "" {
				r.pending = &pendingRotation{nonce: nonce, refreshToken: refresh, at: now}
			}
			return err
		}
		// Any classified answer — even a rejection — means the CP spoke,
		// so there is nothing left to replay.
		r.pending = nil
		return err
	}
	r.pending = nil

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
