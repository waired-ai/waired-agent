package management

import "context"

// LoginPhase enumerates the lifecycle of a daemon-driven login session.
// It is the tray/CLI-facing projection of the agent's enrollment +
// activation progress, so a thin client can render inline status
// without scraping a subprocess's stdout (the old pkexec path).
//
//	unenrolled  — no login in flight and no identity (fresh daemon)
//	logging_in  — control plane minted a login session; browser OAuth
//	              is pending. LoginURL/UserCode are populated.
//	activating  — OAuth completed + device enrolled; the daemon is
//	              bringing the identity-dependent runtime up live.
//	active      — runtime is up; the agent is enrolled and connected.
//	error       — enrollment or activation failed; Error is populated.
type LoginPhase string

const (
	LoginPhaseUnenrolled LoginPhase = "unenrolled"
	LoginPhaseLoggingIn  LoginPhase = "logging_in"
	LoginPhaseActivating LoginPhase = "activating"
	LoginPhaseActive     LoginPhase = "active"
	LoginPhaseError      LoginPhase = "error"
)

// LoginStartRequest is the POST /waired/v1/login/start body. Both fields
// are optional; empty values fall back to the daemon's configured
// defaults (--control flag / $WAIRED_CONTROL_URL and the host name).
type LoginStartRequest struct {
	ControlURL string `json:"control_url,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
}

// LoginStatus is returned by both /waired/v1/login/start and
// /waired/v1/login/status. The client polls status until Phase reaches
// a terminal value (active or error). LoginURL/UserCode appear once the
// control plane mints the session (a tick after start in the common
// case, since OAuth runs on a background goroutine).
type LoginStatus struct {
	SessionID    string     `json:"session_id,omitempty"`
	Phase        LoginPhase `json:"phase"`
	LoginURL     string     `json:"login_url,omitempty"`
	UserCode     string     `json:"user_code,omitempty"`
	AccountEmail string     `json:"account_email,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// LoginController is implemented by the agent. It owns the login session
// (Tailscale-model: the daemon, not a spawned CLI, drives enrollment),
// so the tray/CLI never need polkit elevation. Start is idempotent /
// single-flight: a second Start while a login is in flight returns the
// existing session rather than spawning a second OAuth.
type LoginController interface {
	// Start kicks off (or rejoins) a login session and returns its
	// current status. It returns quickly; the browser OAuth + device
	// enrollment + live activation run on a background goroutine the
	// caller observes via Status.
	Start(ctx context.Context, req LoginStartRequest) (LoginStatus, error)
	// Status reports the current state of the session identified by
	// sessionID. An empty / stale / unknown id yields the daemon's
	// current resting phase (unenrolled or active) rather than an error.
	Status(ctx context.Context, sessionID string) (LoginStatus, error)
}
