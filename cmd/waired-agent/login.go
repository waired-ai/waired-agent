package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// enrollFunc is the enrollment entry point. Defaults to setup.Enroll;
// overridden in tests so the phase progression can be driven without a
// real control plane.
type enrollFunc func(ctx context.Context, opts setup.EnrollOptions) (*setup.EnrollResult, error)

// loginController owns the daemon's at-most-one in-flight login session
// (Tailscale model: the daemon, not a spawned CLI, drives enrollment).
// On success it activates the identity-dependent runtime live via the
// captured activate func — no process restart. It implements
// management.LoginController.
type loginController struct {
	sb       *switchboard
	activate func(parent context.Context) error
	// reactivate tears the live session down and rebuilds it from the
	// state a re-auth just rewrote. activate alone cannot do this: it
	// refuses to publish over a session that is already current, which is
	// precisely the state every re-auth starts from.
	reactivate func(parent context.Context) error
	enroll     enrollFunc
	rootCtx    context.Context

	stateDir          string
	defaultControlURL string
	endpoint          string
	logger            *slog.Logger

	mu      sync.Mutex
	session *loginSession
}

type loginSession struct {
	id           string
	phase        management.LoginPhase
	loginURL     string
	userCode     string
	accountEmail string
	errMsg       string
	cancel       context.CancelFunc
}

type loginControllerConfig struct {
	StateDir          string
	DefaultControlURL string
	Endpoint          string
	RootCtx           context.Context
	Activate          func(parent context.Context) error
	// Reactivate replaces a live session with one built from the state on
	// disk. Required for re-auth; optional otherwise (a nil one makes a
	// re-auth fail loudly rather than half-succeed with stale tokens in
	// the running session).
	Reactivate func(parent context.Context) error
	Logger     *slog.Logger
	// Enroll is optional; nil uses setup.Enroll.
	Enroll enrollFunc
}

func newLoginController(sb *switchboard, cfg loginControllerConfig) *loginController {
	enroll := cfg.Enroll
	if enroll == nil {
		enroll = setup.Enroll
	}
	return &loginController{
		sb:                sb,
		activate:          cfg.Activate,
		reactivate:        cfg.Reactivate,
		enroll:            enroll,
		rootCtx:           cfg.RootCtx,
		stateDir:          cfg.StateDir,
		defaultControlURL: cfg.DefaultControlURL,
		endpoint:          cfg.Endpoint,
		logger:            cfg.Logger,
	}
}

func (lc *loginController) Start(ctx context.Context, req management.LoginStartRequest) (management.LoginStatus, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	// Already enrolled + active: idempotent no-op. `waired init` run twice
	// must not re-enrol, and the tray's start-on-click must not either.
	//
	// Reauth is the one caller that means it. It is how an enrolled device
	// renews credentials the refresh loop can no longer renew for itself
	// (#175) — the control plane matches the machine key and renews the
	// same device row, so this replaces tokens, it does not add a device.
	// Read the session once: lc.mu does not cover the switchboard, and the
	// node-key rotator can swap it underneath. Two reads could disagree and
	// leave this taking the no-op branch with reauth already true.
	live := lc.sb.current() != nil
	reauth := req.Reauth && live
	if live && !req.Reauth {
		return management.LoginStatus{Phase: management.LoginPhaseActive}, nil
	}
	if reauth && lc.reactivate == nil {
		// Enrolling would rewrite the tokens on disk while the live session
		// kept using the old ones — worse than refusing, and invisible.
		return management.LoginStatus{}, errors.New("login: this daemon cannot re-authenticate a live session")
	}
	// A login is already in flight: single-flight — return its status
	// rather than spawning a second browser OAuth.
	if lc.session != nil {
		switch lc.session.phase {
		case management.LoginPhaseLoggingIn, management.LoginPhaseActivating:
			return lc.snapshotLocked(), nil
		}
	}

	controlURL := req.ControlURL
	if controlURL == "" {
		controlURL = lc.defaultControlURL
	}
	if controlURL == "" {
		return management.LoginStatus{}, errors.New("login: no control URL (start the agent with --control / $WAIRED_CONTROL_URL, or pass control_url)")
	}
	deviceName := req.DeviceName
	if deviceName == "" {
		host, _ := os.Hostname()
		deviceName = host
	}

	sessID := newLoginSessionID()
	// The OAuth poll + activation run for minutes; derive their context
	// from the process-lifetime rootCtx, NOT the (millisecond-lived) HTTP
	// request ctx.
	loginCtx, cancel := context.WithCancel(lc.rootCtx)
	lc.session = &loginSession{
		id:     sessID,
		phase:  management.LoginPhaseLoggingIn,
		cancel: cancel,
	}
	go lc.run(loginCtx, sessID, controlURL, deviceName, req.AuthKey, reauth)

	return lc.snapshotLocked(), nil
}

func (lc *loginController) Status(ctx context.Context, sessionID string) (management.LoginStatus, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if lc.session != nil && lc.session.id == sessionID {
		return lc.snapshotLocked(), nil
	}
	// Unknown / stale / empty session id: report the daemon's resting
	// phase instead of erroring, so a late poll degrades gracefully.
	if lc.sb.current() != nil {
		return management.LoginStatus{Phase: management.LoginPhaseActive}, nil
	}
	return management.LoginStatus{Phase: management.LoginPhaseUnenrolled}, nil
}

// run executes enrollment then live activation on a background
// goroutine, advancing the session's phase as it goes. reauth selects
// the activation that replaces a live session instead of publishing a
// first one.
func (lc *loginController) run(ctx context.Context, sessID, controlURL, deviceName, authKey string, reauth bool) {
	// Resolve a port-0 login endpoint (default "udp4:127.0.0.1:0") to a
	// concrete free UDP port before enrolling. The endpoint is persisted into
	// identity.json and later parsed by udpListenPortFromEndpoint (which
	// rejects port 0) and bound by the WireGuard engine, so a bare ":0" from
	// --login-listen would otherwise fail activation. No-op when the service
	// already passed a concrete port (as the Linux systemd unit does).
	endpoint, err := resolveLoginEndpoint(lc.endpoint)
	if err != nil {
		lc.fail(sessID, fmt.Errorf("resolve login endpoint %q: %w", lc.endpoint, err))
		return
	}
	res, err := lc.enroll(ctx, setup.EnrollOptions{
		ControlURL:    controlURL,
		DeviceName:    deviceName,
		Endpoint:      endpoint,
		StateDir:      lc.stateDir,
		ClientVersion: buildinfo.Version,
		// #175: when set, setup.Enroll redeems the key in the login-session
		// create call and never reaches OnLoginURL below — there is no URL
		// for anyone to open.
		AuthKey: authKey,
		OnLoginURL: func(loginURL, userCode string) {
			lc.mu.Lock()
			if lc.session != nil && lc.session.id == sessID {
				lc.session.loginURL = loginURL
				lc.session.userCode = userCode
			}
			lc.mu.Unlock()
		},
	})
	if err != nil {
		lc.fail(sessID, err)
		return
	}

	lc.mu.Lock()
	if lc.session != nil && lc.session.id == sessID {
		lc.session.phase = management.LoginPhaseActivating
		lc.session.accountEmail = res.AccountEmail
	}
	lc.mu.Unlock()

	// Live activation. Runs on rootCtx (process lifetime): the resulting
	// session must outlive both this goroutine and the login context.
	// A re-auth has a session already running on the credentials we just
	// replaced, so it tears that down first — activate refuses to publish
	// over a live one, and leaving it up would mean the daemon kept using
	// tokens the control plane has already rotated away from.
	activate := lc.activate
	if reauth {
		activate = lc.reactivate
	}
	if err := activate(lc.rootCtx); err != nil {
		lc.fail(sessID, err)
		return
	}

	lc.mu.Lock()
	if lc.session != nil && lc.session.id == sessID {
		lc.session.phase = management.LoginPhaseActive
	}
	lc.mu.Unlock()
}

func (lc *loginController) fail(sessID string, err error) {
	lc.mu.Lock()
	if lc.session != nil && lc.session.id == sessID {
		lc.session.phase = management.LoginPhaseError
		lc.session.errMsg = err.Error()
	}
	lc.mu.Unlock()
	lc.logger.Error("daemon-driven login failed", "session", sessID, "err", err)
}

// snapshotLocked builds the wire status from the current session. The
// caller must hold lc.mu.
func (lc *loginController) snapshotLocked() management.LoginStatus {
	s := lc.session
	if s == nil {
		return management.LoginStatus{Phase: management.LoginPhaseUnenrolled}
	}
	return management.LoginStatus{
		SessionID:    s.id,
		Phase:        s.phase,
		LoginURL:     s.loginURL,
		UserCode:     s.userCode,
		AccountEmail: s.accountEmail,
		Error:        s.errMsg,
	}
}

func newLoginSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
