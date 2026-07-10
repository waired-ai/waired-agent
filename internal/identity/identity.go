// Package identity manages the on-disk state a Waired agent needs to
// keep across restarts: the result of `waired init` (account / network /
// device IDs, control plane URL, signing key fingerprint) plus the
// secret artifacts (Machine Key, Node Key, Device Access Token, cached
// Device Certificate, cached Network Map).
//
// State layout under StateDir (typically ~/.config/waired):
//
//	identity.json                         (0644)
//	secrets/machine.key                   (0600, raw 64-byte ed25519 priv)
//	secrets/node.key                      (0600, raw 32-byte x25519 priv)
//	secrets/access_token                  (0600, opaque CP token)
//	secrets/refresh_token                 (0600, opaque CP refresh token)
//	cache/control-signing-key.ed25519     (0644, 32-byte CP signing pub)
//	cache/device_certificate.json         (0644, signed cert from CP)
//	cache/network_map.json                (0644, last received signed map)
//	cache/token_meta.json                 (0644, {access_expires_at, device_auth_expires_at})
package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// Paths bundles all the file paths relative to a StateDir.
type Paths struct {
	StateDir             string
	Identity             string // identity.json
	MachineKey           string // secrets/machine.key
	NodeKey              string // secrets/node.key
	NodeKeyNext          string // secrets/node.key.next (staged during rotation)
	AccessToken          string // secrets/access_token
	RefreshToken         string // secrets/refresh_token
	ControlSigningPubKey string // cache/control-signing-key.ed25519
	DeviceCertificate    string // cache/device_certificate.json
	NetworkMap           string // cache/network_map.json
	TokenMeta            string // cache/token_meta.json
	NodeKeyMeta          string // cache/node_key_meta.json
}

// PathsFor returns Paths under stateDir, creating the directory tree
// with the platform-appropriate protection (POSIX 0700 on Unix, NTFS
// DACL locking down to SYSTEM/Administrators/current-user on Windows).
// Write paths (Save*) use this; read paths use PathsUnder.
func PathsFor(stateDir string) (*Paths, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"", "secrets", "cache"} {
		dir := filepath.Join(stateDir, sub)
		if err := secrets.SecureDir(dir); err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
	}
	return p, nil
}

// PathsUnder returns the Paths layout under stateDir without touching
// the filesystem. Read paths (Load*) use this so a status query never
// creates or re-permissions directories — a non-root read of a
// root-owned state dir must surface the real EACCES from ReadFile, not
// a chmod EPERM from SecureDir.
func PathsUnder(stateDir string) (*Paths, error) {
	if stateDir == "" {
		return nil, errors.New("identity: empty state dir")
	}
	return &Paths{
		StateDir:             stateDir,
		Identity:             filepath.Join(stateDir, "identity.json"),
		MachineKey:           filepath.Join(stateDir, "secrets", "machine.key"),
		NodeKey:              filepath.Join(stateDir, "secrets", "node.key"),
		NodeKeyNext:          filepath.Join(stateDir, "secrets", "node.key.next"),
		AccessToken:          filepath.Join(stateDir, "secrets", "access_token"),
		RefreshToken:         filepath.Join(stateDir, "secrets", "refresh_token"),
		ControlSigningPubKey: filepath.Join(stateDir, "cache", "control-signing-key.ed25519"),
		DeviceCertificate:    filepath.Join(stateDir, "cache", "device_certificate.json"),
		NetworkMap:           filepath.Join(stateDir, "cache", "network_map.json"),
		TokenMeta:            filepath.Join(stateDir, "cache", "token_meta.json"),
		NodeKeyMeta:          filepath.Join(stateDir, "cache", "node_key_meta.json"),
	}, nil
}

// Identity is the persisted result of a successful `waired init`. Every
// field comes back from the CP in the EnrollDeviceResponse, except the
// ControlURL and Endpoint which the agent chose at init time and pinned.
type Identity struct {
	DeviceID                string `json:"device_id"`
	DeviceName              string `json:"device_name,omitempty"` // human-readable; falls back to DeviceID for older identity.json
	NetworkID               string `json:"network_id"`
	NetworkName             string `json:"network_name"`
	AccountID               string `json:"account_id"`
	AccountEmail            string `json:"account_email"`
	OverlayIP               string `json:"overlay_ip"`
	Endpoint                string `json:"endpoint"` // e.g., "udp4:host:port"
	ControlURL              string `json:"control_url"`
	ControlSigningPublicKey string `json:"control_signing_public_key"`
}

// Load reads identity.json. Returns (nil, nil) when the file does not
// exist - the caller decides whether that's an error (run `waired init`).
func Load(stateDir string) (*Identity, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p.Identity)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("identity: read %s: %w", p.Identity, err)
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("identity: parse %s: %w", p.Identity, err)
	}
	return &id, nil
}

// Save writes identity.json atomically with NonSecret protection
// (world-readable on Unix; default DACL on Windows).
func Save(stateDir string, id *Identity) error {
	p, err := PathsFor(stateDir)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return secrets.WriteFile(p.Identity, body, secrets.NonSecret)
}

// accessTokenItem / refreshTokenItem are the Keychain identities for the CP
// session tokens, stored in the macOS Keychain on darwin via securestore
// and falling back to the 0600 file elsewhere (#261).
func accessTokenItem() keychain.Item {
	return keychain.Item{Account: securestore.Account, Service: securestore.ServiceAccessToken}
}

func refreshTokenItem() keychain.Item {
	return keychain.Item{Account: securestore.Account, Service: securestore.ServiceRefreshToken}
}

// SaveAccessToken writes the opaque CP access token to the macOS Keychain
// on darwin (falling back to a 0600 file elsewhere, via securestore).
func SaveAccessToken(stateDir, token string) error {
	p, err := PathsFor(stateDir)
	if err != nil {
		return err
	}
	return securestore.Write(accessTokenItem(), p.AccessToken, []byte(token))
}

// LoadAccessToken reads the access token. Returns ("", nil) if missing.
func LoadAccessToken(stateDir string) (string, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return "", err
	}
	data, err := securestore.Read(accessTokenItem(), p.AccessToken)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// SaveRefreshToken writes the opaque CP refresh token with the same
// Keychain/file protection as the access token. The refresh token is
// long-lived (device_auth_expires_at ≈ 180 days) and lets the agent
// rotate the short-lived access token without re-OAuth.
func SaveRefreshToken(stateDir, token string) error {
	p, err := PathsFor(stateDir)
	if err != nil {
		return err
	}
	return securestore.Write(refreshTokenItem(), p.RefreshToken, []byte(token))
}

// LoadRefreshToken reads the refresh token. Returns ("", nil) if
// missing — older enrollments (pre-Phase-A CP) never wrote one.
func LoadRefreshToken(stateDir string) (string, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return "", err
	}
	data, err := securestore.Read(refreshTokenItem(), p.RefreshToken)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// TokenMeta is the non-secret bookkeeping that pairs with the secret
// access_token / refresh_token files. The agent's auto-refresh loop
// reads AccessExpiresAt to schedule the next refresh, and reads
// DeviceAuthExpiresAt to know when re-OAuth is required (refresh
// alone won't suffice past this point).
//
// ReauthRequiredAt is set when the agent has learned (from a 401 with
// error.type=reauth_required, or from a terminal refresh-token
// classification like reuse_detected) that the CP no longer trusts the
// device's credentials. Once set, the refresh loop stops trying and
// `waired auth status` surfaces the state. Cleared on successful
// re-enrollment via `waired init`.
type TokenMeta struct {
	AccessExpiresAt     time.Time `json:"access_expires_at"`
	DeviceAuthExpiresAt time.Time `json:"device_auth_expires_at"`
	ReauthRequiredAt    time.Time `json:"reauth_required_at,omitzero"`
}

// NeedsReauth reports whether the on-disk meta says the device must
// be re-enrolled (i.e. ReauthRequiredAt is set). Centralised here so
// callers don't open-code the zero-value check.
func (m TokenMeta) NeedsReauth() bool {
	return !m.ReauthRequiredAt.IsZero()
}

// SaveTokenMeta writes cache/token_meta.json with NonSecret protection.
// The expiries themselves are not secrets; the cleartext tokens live
// in secrets/.
func SaveTokenMeta(stateDir string, m TokenMeta) error {
	p, err := PathsFor(stateDir)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return secrets.WriteFile(p.TokenMeta, body, secrets.NonSecret)
}

// LoadTokenMeta reads cache/token_meta.json. Returns (zero-value, nil)
// when the file is missing — older agents predate this file; the
// refresh loop treats that as "valid until 401".
func LoadTokenMeta(stateDir string) (TokenMeta, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return TokenMeta{}, err
	}
	data, err := os.ReadFile(p.TokenMeta)
	if err != nil {
		if os.IsNotExist(err) {
			return TokenMeta{}, nil
		}
		return TokenMeta{}, err
	}
	var m TokenMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return TokenMeta{}, fmt.Errorf("identity: parse %s: %w", p.TokenMeta, err)
	}
	return m, nil
}

// NodeKeyMeta is the non-secret bookkeeping for the device's current
// Node Key (#228). The rotation loop reads ExpiresAt to schedule the
// next rotation (at ExpiresAt - signer.NodeKeyRotationLead) and to know
// when the key has hard-expired. IssuedAt is informational. Seeded from
// the enroll response and refreshed from every refresh / rotate response.
type NodeKeyMeta struct {
	IssuedAt  time.Time `json:"issued_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SaveNodeKeyMeta writes cache/node_key_meta.json (NonSecret).
func SaveNodeKeyMeta(stateDir string, m NodeKeyMeta) error {
	p, err := PathsFor(stateDir)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return secrets.WriteFile(p.NodeKeyMeta, body, secrets.NonSecret)
}

// LoadNodeKeyMeta reads cache/node_key_meta.json. Returns (zero, nil)
// when the file is missing (older agents predate it; the rotation loop
// waits for a refresh to populate it before scheduling).
func LoadNodeKeyMeta(stateDir string) (NodeKeyMeta, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return NodeKeyMeta{}, err
	}
	data, err := os.ReadFile(p.NodeKeyMeta)
	if err != nil {
		if os.IsNotExist(err) {
			return NodeKeyMeta{}, nil
		}
		return NodeKeyMeta{}, err
	}
	var m NodeKeyMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return NodeKeyMeta{}, fmt.Errorf("identity: parse %s: %w", p.NodeKeyMeta, err)
	}
	return m, nil
}

// SaveBytes writes raw bytes to one of the cache files. The perm
// argument is interpreted as a sensitivity hint: any mode that
// excludes group/other read (e.g. 0o600) is treated as a Secret and
// receives the platform-appropriate strict protection; everything else
// is written as NonSecret.
func SaveBytes(path string, data []byte, perm os.FileMode) error {
	sens := secrets.NonSecret
	if perm&0o077 == 0 {
		sens = secrets.Secret
	}
	return secrets.WriteFile(path, data, sens)
}
