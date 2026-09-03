// Package setup holds the pieces of first-run setup that are shared
// between the daemon and the CLI:
//
//	Enroll       sign-in / device key generation / CP registration.
//	             Called by the daemon's login controller (cmd/waired-agent),
//	             which owns enrollment since #175.
//	Integration  Claude Code / OpenClaw auto-config. Called by
//	             `waired link`, `waired doctor`, and the post-login
//	             integration step of a daemon-driven `waired init`.
//	DetectOllama / SelectBundledModel
//	             engine + model decisions, read by both sides.
//
// It used to also hold Init and Deploy — a second, in-process
// implementation of the whole journey that `waired init` ran itself.
// That is gone (#175): a device the CLI enrolled never declared the agent
// capabilities the control plane learns from the daemon's network-map
// poll, so it enrolled "successfully" into a setup that could not finish.
package setup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
)

// EnrollOptions captures the (already-resolved) inputs phase 1 needs.
// Resolution of defaults / hostname fallback is the caller's job
// (cmd/waired/main.go).
type EnrollOptions struct {
	ControlURL      string
	DeviceName      string
	Endpoint        string
	StateDir        string
	HTTPClient      *http.Client // nil = default
	OnLoginURL      func(loginURL, userCode string)
	OnLoginComplete func(accountEmail, networkName string)
	// OnLoginExpiry reports the control plane's window for this sign-in.
	// Threaded so the terminal's own deadline can be the server's number
	// rather than a second copy of it (waired-agent#1175).
	OnLoginExpiry func(expiresAt time.Time)
	ClientVersion string
	// AuthKey enrolls with an unattended-enrollment credential instead of
	// a browser sign-in (#175, waired#976). When set, RunInit skips
	// OnLoginURL and the poll loop entirely — the Control Plane authorizes
	// the session in the create call and hands back the ticket there.
	AuthKey string
}

// EnrollResult is what callers print after a successful enroll. It
// is intentionally a subset of controlclient.InitResult so callers
// don't reach into the controlclient package.
type EnrollResult struct {
	DeviceID     string
	NetworkName  string
	NetworkID    string
	OverlayIP    string
	AccountEmail string
}

// Enroll runs phase 1 (steps 1–4): generate / load device keys, talk
// to the Control Plane, persist identity + access token + cert.
//
// Errors propagate verbatim — Init's fail-fast policy is at the
// orchestrator level.
func Enroll(ctx context.Context, opts EnrollOptions) (*EnrollResult, error) {
	if opts.ControlURL == "" {
		return nil, errors.New("setup: empty control URL")
	}
	if opts.StateDir == "" {
		return nil, errors.New("setup: empty state dir")
	}
	if opts.DeviceName == "" {
		host, _ := os.Hostname()
		opts.DeviceName = host
	}
	if opts.Endpoint == "" {
		return nil, errors.New("setup: empty endpoint")
	}
	// No ClientVersion default. It used to substitute "0.1.0", which is a
	// version this project has never shipped and which a fleet view cannot
	// tell apart from a genuinely ancient agent — the placeholder has since
	// spread into fixtures and specs as if it were real (waired-agent#655).
	// Nothing can reach it anyway: the only non-test caller is
	// cmd/waired-agent's login path, which passes buildinfo.Version, and
	// that defaults to "0.0.0-dev" rather than empty. A caller that leaves
	// it empty now reports empty, which is at least true.

	paths, err := identity.PathsFor(opts.StateDir)
	if err != nil {
		return nil, err
	}
	mk, err := devicekeys.LoadOrCreateMachineKey(paths.MachineKey)
	if err != nil {
		return nil, fmt.Errorf("setup: machine key: %w", err)
	}
	nk, err := devicekeys.LoadOrCreateNodeKey(paths.NodeKey)
	if err != nil {
		return nil, fmt.Errorf("setup: node key: %w", err)
	}

	res, err := controlclient.RunInit(ctx, controlclient.InitParams{
		ControlURL:      opts.ControlURL,
		DeviceName:      opts.DeviceName,
		Platform:        runtime.GOOS,
		Arch:            runtime.GOARCH,
		ClientVersion:   opts.ClientVersion,
		Endpoint:        opts.Endpoint,
		MachineKey:      mk,
		NodeKey:         nk,
		OnLoginURL:      opts.OnLoginURL,
		OnLoginComplete: opts.OnLoginComplete,
		OnLoginExpiry:   opts.OnLoginExpiry,
		HTTPClient:      opts.HTTPClient,
		AuthKey:         opts.AuthKey,
	})
	if err != nil {
		return nil, err
	}

	// The name the control plane assigned, not the one this machine
	// reported. They differ when a second machine shares this hostname
	// (the CP suffixes it) and after a rename in the web console
	// (waired-ai/waired#1204). Storing the reported one made `waired auth
	// status` show a name nobody else used, and `waired init` re-sent it
	// (#767). Falls back for a control plane that predates the field,
	// where the reported hostname still is the name.
	deviceName := res.DeviceName
	if deviceName == "" {
		deviceName = opts.DeviceName
	}
	if err := identity.Save(opts.StateDir, &identity.Identity{
		DeviceID:                res.DeviceID,
		DeviceName:              deviceName,
		NetworkID:               res.NetworkID,
		NetworkName:             res.NetworkName,
		AccountID:               res.AccountID,
		AccountEmail:            res.AccountEmail,
		OverlayIP:               res.OverlayIP,
		Endpoint:                opts.Endpoint,
		ControlURL:              opts.ControlURL,
		ControlSigningPublicKey: res.ControlSigningPublicKey,
	}); err != nil {
		return nil, fmt.Errorf("setup: save identity: %w", err)
	}
	if err := identity.SaveAccessToken(opts.StateDir, res.DeviceAccessToken); err != nil {
		return nil, fmt.Errorf("setup: save access token: %w", err)
	}
	if res.DeviceRefreshToken != "" {
		if err := identity.SaveRefreshToken(opts.StateDir, res.DeviceRefreshToken); err != nil {
			return nil, fmt.Errorf("setup: save refresh token: %w", err)
		}
	}
	if err := identity.SaveTokenMeta(opts.StateDir, identity.TokenMeta{
		AccessExpiresAt:     res.DeviceAccessTokenExpiresAt,
		DeviceAuthExpiresAt: res.DeviceAuthExpiresAt,
	}); err != nil {
		return nil, fmt.Errorf("setup: save token meta: %w", err)
	}
	// Seed the Node Key rotation clock (#228) so the agent's rotation loop
	// knows when to rotate. Zero when talking to a pre-#228 CP — the loop
	// then waits for a refresh to populate it.
	if !res.NodeKeyExpiresAt.IsZero() {
		if err := identity.SaveNodeKeyMeta(opts.StateDir, identity.NodeKeyMeta{
			IssuedAt:  time.Now().UTC(),
			ExpiresAt: res.NodeKeyExpiresAt,
		}); err != nil {
			return nil, fmt.Errorf("setup: save node-key meta: %w", err)
		}
	}
	if err := identity.SaveBytes(paths.DeviceCertificate, res.DeviceCertificateJSON, 0o644); err != nil {
		return nil, fmt.Errorf("setup: save device cert: %w", err)
	}
	if pub, err := base64.StdEncoding.DecodeString(res.ControlSigningPublicKey); err == nil {
		_ = identity.SaveBytes(paths.ControlSigningPubKey, pub, 0o644)
	}

	return &EnrollResult{
		DeviceID:     res.DeviceID,
		NetworkName:  res.NetworkName,
		NetworkID:    res.NetworkID,
		OverlayIP:    res.OverlayIP,
		AccountEmail: res.AccountEmail,
	}, nil
}
