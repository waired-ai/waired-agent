package main

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/update"
)

// updateCacheTTL bounds how often Check hits the version feed. The tray
// polls /update/status (cheap, cached) every few seconds and POSTs
// /update/check on startup; without a TTL a busy tray would hammer the
// GitHub API. Force bypasses it (a manual `waired update --check`).
const updateCacheTTL = 6 * time.Hour

// Background-check cadence (#294). The loop ticks at updateCheckInterval
// (== updateCacheTTL, so each tick is a genuine refresh rather than a cache
// hit) and runs an initial check shortly after boot. The initial delay keeps
// startup snappy; updateCheckJitterMax desynchronises a fleet so many agents
// behind one NAT don't fire their GitHub-API checks at the same instant
// (Linux uses the local apt cache and hits no API at all).
const (
	updateCheckInterval     = 6 * time.Hour
	updateCheckInitialDelay = 1 * time.Minute
	updateCheckJitterMax    = 60 * time.Second
)

// updateController implements management.UpdateController. The check is
// read-only: it resolves the latest published version and compares it against
// the running build (buildinfo.Version). The apply itself is the CLI/tray's
// job (they re-run the installer under elevation) — the unprivileged daemon
// never installs anything. runUpdateCheckLoop drives it on a timer (#294);
// SetNotify persists the operator's update-prompt preference.
type updateController struct {
	current string
	check   func(ctx context.Context, current string) (update.Result, error)
	now     func() time.Time

	mu        sync.Mutex
	cached    management.UpdateStatus
	checkedAt time.Time
	hasResult bool

	// notifyEnabled is the operator's "prompt me about updates" preference
	// (#294). Overlaid onto every returned status so the latest toggle value
	// is reflected regardless of when the cached check ran. persistNotify
	// writes it through to <state-dir>/runtime/desired-update-notify; nil in
	// unit tests that don't exercise persistence.
	notifyEnabled bool
	persistNotify func(enabled bool) error

	// lastLoggedVersion dedupes the background loop's "update available" log
	// so a headless agent logs each new version once, not every tick.
	lastLoggedVersion string
}

// newUpdateController builds the daemon-side controller. It seeds the
// update-prompt preference from <state-dir>/runtime/desired-update-notify
// (default ON; a read error degrades to ON rather than silently muting
// prompts) and wires persistNotify to write the operator's toggle back.
func newUpdateController(stateDir string) *updateController {
	r := &update.Resolver{}
	notify, err := state.ReadDesiredUpdateNotify(stateDir)
	enabled := err != nil || notify.Enabled()
	return &updateController{
		current:       buildinfo.Version,
		check:         r.Check,
		now:           time.Now,
		notifyEnabled: enabled,
		persistNotify: func(on bool) error {
			s := state.UpdateNotifyOn
			if !on {
				s = state.UpdateNotifyOff
			}
			return state.WriteDesiredUpdateNotify(stateDir, s)
		},
	}
}

// Check refreshes the cached result unless a fresh one exists and Force is
// false. A feed failure is reported as Phase=error in the returned status
// (HTTP 200) rather than a transport-level error, and does not clobber the
// last good cached result — a transient GitHub rate-limit shouldn't blank
// the tray banner.
func (c *updateController) Check(ctx context.Context, req management.UpdateCheckRequest) (management.UpdateStatus, error) {
	c.mu.Lock()
	if !req.Force && c.hasResult && c.now().Sub(c.checkedAt) < updateCacheTTL {
		st := c.cached
		st.NotifyEnabled = c.notifyEnabled
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	res, err := c.check(ctx, c.current)
	st := management.UpdateStatus{
		CurrentVersion: c.current,
		ApplyMethod:    applyMethod(),
		CheckedAt:      c.now().UTC().Format(time.RFC3339),
	}
	if err != nil {
		st.Phase = management.UpdatePhaseError
		st.Error = err.Error()
		c.mu.Lock()
		st.NotifyEnabled = c.notifyEnabled
		c.mu.Unlock()
		return st, nil // keep prior cache for Status()
	}
	st.LatestVersion = res.Latest
	st.Available = res.Available
	if res.Available {
		st.Phase = management.UpdatePhaseAvailable
	} else {
		st.Phase = management.UpdatePhaseIdle
	}
	c.mu.Lock()
	c.cached = st
	c.checkedAt = c.now()
	c.hasResult = true
	st.NotifyEnabled = c.notifyEnabled
	c.mu.Unlock()
	return st, nil
}

// Status returns the last cached check result, or an idle status (no check
// yet) — never forcing a network hit, so the tray can poll it freely. The
// current notify preference is always overlaid.
func (c *updateController) Status(_ context.Context) (management.UpdateStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasResult {
		st := c.cached
		st.NotifyEnabled = c.notifyEnabled
		return st, nil
	}
	return management.UpdateStatus{
		Phase:          management.UpdatePhaseIdle,
		CurrentVersion: c.current,
		ApplyMethod:    applyMethod(),
		NotifyEnabled:  c.notifyEnabled,
	}, nil
}

// SetNotify persists the operator's update-prompt preference and returns the
// refreshed status. The disk write happens before the in-memory flip so a
// failed write leaves memory consistent with disk (the toggle simply did not
// take). The check result is unaffected — only whether the tray prompts.
func (c *updateController) SetNotify(_ context.Context, enabled bool) (management.UpdateStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistNotify != nil {
		if err := c.persistNotify(enabled); err != nil {
			return management.UpdateStatus{}, err
		}
	}
	c.notifyEnabled = enabled
	var st management.UpdateStatus
	if c.hasResult {
		st = c.cached
	} else {
		st = management.UpdateStatus{
			Phase:          management.UpdatePhaseIdle,
			CurrentVersion: c.current,
			ApplyMethod:    applyMethod(),
		}
	}
	st.NotifyEnabled = enabled
	return st, nil
}

// runUpdateCheckLoop periodically refreshes the daemon's update status so a
// release published after boot surfaces on /update/status (and the tray
// prompt) without a client having to POST /update/check, and so headless
// agents — which have no tray to seed a check — still detect updates. It is
// identity-independent (wired in run(), not a session) and stops on ctx.
//
// Each tick calls Check with Force=false, cooperating with the 6h TTL: a
// manual `waired update` between ticks adds no extra feed hit, and the first
// tick after the TTL lapses does the real refresh. On Linux the resolver
// reads the local apt cache (no GitHub API); only Windows/macOS query the
// GitHub Releases API (~4 calls/day/agent, far under the 60/hr limit).
func runUpdateCheckLoop(ctx context.Context, uc *updateController, interval time.Duration, logger *slog.Logger) {
	if uc == nil || interval <= 0 {
		return
	}
	if logger != nil {
		logger.Debug("update check loop started", "interval", interval.String())
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(updateCheckInitialDelay + updateCheckJitter()):
	}
	uc.checkAndLog(ctx, logger)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			uc.checkAndLog(ctx, logger)
		}
	}
}

// checkAndLog runs one cache-cooperating check and emits a deduped INFO log
// the first time a given newer version is seen. The log matters on headless
// agents (no tray to toast); the tray itself prompts off /update/status.
func (c *updateController) checkAndLog(ctx context.Context, logger *slog.Logger) {
	st, _ := c.Check(ctx, management.UpdateCheckRequest{}) // Check folds feed errors into st; never a Go err
	if logger != nil {
		logger.Debug("update check complete",
			"phase", string(st.Phase),
			"available", st.Available,
			"current", st.CurrentVersion,
			"latest", st.LatestVersion,
		)
	}
	if st.Phase != management.UpdatePhaseAvailable || st.LatestVersion == "" {
		return
	}
	c.mu.Lock()
	fresh := st.LatestVersion != c.lastLoggedVersion
	c.lastLoggedVersion = st.LatestVersion
	c.mu.Unlock()
	if fresh && logger != nil {
		logger.Info("waired update available",
			"current", st.CurrentVersion,
			"latest", st.LatestVersion,
			"apply", st.ApplyMethod)
	}
}

// updateCheckJitter returns a per-host startup jitter derived from the PID so
// agents on the same network don't synchronise their first GitHub-API check.
// PID-derived (not random) keeps it allocation- and dependency-free; the only
// goal is spreading the fleet, for which process-to-process variance suffices.
func updateCheckJitter() time.Duration {
	return time.Duration(os.Getpid()%int(updateCheckJitterMax/time.Second)) * time.Second
}

// applyMethod maps the daemon's OS to the apply mechanism the thin client
// should drive (matches packaging/install/install.{sh,ps1}).
func applyMethod() string {
	switch runtime.GOOS {
	case "windows":
		return "installer" // install.ps1 two-phase elevated swap
	case "darwin":
		return "installsh" // install.sh under administrator privileges
	default:
		return "apt" // install.sh apt --only-upgrade (Linux)
	}
}
