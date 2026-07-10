package management

import "context"

// UpdatePhase enumerates the daemon-side check state. The daemon only
// *checks* (it runs unprivileged and cannot install); the actual apply is
// driven by the CLI (`waired update`) and tray under elevation, delegating
// to the installer scripts. So the daemon's phase never advances past
// available/idle/error.
//
//	idle      — no newer version, or no check has run yet
//	checking  — a feed query is in flight
//	available — a newer release than the running build is published
//	error     — the last check failed; Error is populated
type UpdatePhase string

const (
	UpdatePhaseIdle      UpdatePhase = "idle"
	UpdatePhaseChecking  UpdatePhase = "checking"
	UpdatePhaseAvailable UpdatePhase = "available"
	UpdatePhaseError     UpdatePhase = "error"
)

// UpdateStatus is returned by POST /waired/v1/update/check and
// GET /waired/v1/update/status. CurrentVersion is the running agent's
// buildinfo.Version; LatestVersion comes from the feed (apt on Linux,
// GitHub Releases elsewhere). ApplyMethod tells the thin client how apply
// works on this OS so it renders the right hint/action:
//
//	apt        — Linux: `waired update` re-runs install.sh (apt --only-upgrade)
//	installer  — Windows: install.ps1 two-phase elevated swap
//	installsh  — macOS: install.sh under administrator privileges
type UpdateStatus struct {
	Phase          UpdatePhase `json:"phase"`
	Available      bool        `json:"available"`
	CurrentVersion string      `json:"current_version"`
	LatestVersion  string      `json:"latest_version,omitempty"`
	ApplyMethod    string      `json:"apply_method,omitempty"`
	CheckedAt      string      `json:"checked_at,omitempty"` // RFC3339; "" if never checked
	Error          string      `json:"error,omitempty"`
	// NotifyEnabled is the operator's "prompt me when an update is
	// available" preference (#294), persisted daemon-side. Deliberately
	// NOT omitempty: a false value must reach the tray so it can suppress
	// the proactive toast — omitempty would make "off" indistinguishable
	// from a legacy daemon that never sent the field.
	NotifyEnabled bool `json:"notify_enabled"`
}

// UpdateCheckRequest is the optional POST /waired/v1/update/check body.
// Force bypasses the daemon's cached result and re-queries the feed.
type UpdateCheckRequest struct {
	Force bool `json:"force,omitempty"`
}

// UpdateSettingsRequest is the POST /waired/v1/update/settings body. It
// toggles whether the tray proactively prompts the user about an available
// update (#294). The check itself always runs; this only gates the prompt.
type UpdateSettingsRequest struct {
	Notify bool `json:"notify"`
}

// UpdateController is implemented by the agent. Check / Status are read-only
// (a feed query + a version compare) and need no privilege — the apply is
// the client's job. Check refreshes (subject to a cache TTL unless Force);
// Status returns the last cached result without forcing a network hit, so
// the tray can poll it cheaply. SetNotify persists the update-prompt
// preference and returns the refreshed status.
type UpdateController interface {
	Check(ctx context.Context, req UpdateCheckRequest) (UpdateStatus, error)
	Status(ctx context.Context) (UpdateStatus, error)
	SetNotify(ctx context.Context, enabled bool) (UpdateStatus, error)
}
