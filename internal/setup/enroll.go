// Package setup is the orchestrator behind `waired init` and the
// shared targets `waired link` / `waired deploy` re-run individually.
//
// Phases (mapping to docs/specs/waired_product_spec.md §5.1 step numbers):
//
//	enroll       (1–4)  : Google sign-in / device key gen / CP register
//	deploy       (5–9)  : hardware profile, runtime/model placement, gateway up
//	integration  (10)   : Claude Code / OpenCode auto-config (transparent proxy + skills + OpenCode plugin)
//
// `waired init` runs all three sequentially and fails-fast on any
// non-skipped error so the user does not have to debug a half-set-up
// install. `--skip-deploy` and `--skip-integration` opt out of phases
// 2 and 3 respectively.
//
// `waired link` re-runs phase 3 only. A future `waired deploy` will
// re-run phase 2 only.
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
	ClientVersion   string
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
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0.1.0"
	}

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
		HTTPClient:      opts.HTTPClient,
	})
	if err != nil {
		return nil, err
	}

	if err := identity.Save(opts.StateDir, &identity.Identity{
		DeviceID:                res.DeviceID,
		DeviceName:              opts.DeviceName,
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
