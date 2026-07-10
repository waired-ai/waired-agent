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
	enroll   enrollFunc
	rootCtx  context.Context

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
	Logger            *slog.Logger
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

	// Already enrolled + active: idempotent no-op.
	if lc.sb.current() != nil {
		return management.LoginStatus{Phase: management.LoginPhaseActive}, nil
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
	go lc.run(loginCtx, sessID, controlURL, deviceName)

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
// goroutine, advancing the session's phase as it goes.
func (lc *loginController) run(ctx context.Context, sessID, controlURL, deviceName string) {
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
	if err := lc.activate(lc.rootCtx); err != nil {
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
