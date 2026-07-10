// Package devicekeys generates and persists the two key types each
// Waired device needs:
//
//   - MachineKey (Ed25519): identifies the device install. Used for
//     Control Plane and Relay handshake signatures. Long-lived.
//   - NodeKey (X25519, WireGuard-compatible): peer-to-peer data plane
//     encryption key. Rotates periodically (step4+).
//
// Both keys are stored as opaque binary blobs with strict 0600 perms.
// Public-key serialisation uses standard base64 to match what the
// Control Plane wire format expects.
package devicekeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/curve25519"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// ---- Machine Key (Ed25519) ----

// MachineKey is the Ed25519 keypair used to sign Control Plane and Relay
// handshake transcripts.
type MachineKey struct {
	Public  ed25519.PublicKey  // 32 bytes
	Private ed25519.PrivateKey // 64 bytes
}

// NewMachineKey generates a fresh Ed25519 keypair.
func NewMachineKey() (*MachineKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("devicekeys: ed25519 gen: %w", err)
	}
	return &MachineKey{Public: pub, Private: priv}, nil
}

// machineKeyItem is the Keychain identity for the device MachineKey. The
// MachineKey is the device's long-lived identity (losing it forces
// re-enrollment), so it is stored in the macOS Keychain on darwin via
// securestore, falling back to the 0600 file elsewhere (#261).
func machineKeyItem() keychain.Item {
	return keychain.Item{Account: securestore.Account, Service: securestore.ServiceMachineKey}
}

// LoadOrCreateMachineKey returns the key, preferring the macOS Keychain and
// falling back to the 0600 file at path. Generates and persists one (to
// both stores) if neither has it.
func LoadOrCreateMachineKey(path string) (*MachineKey, error) {
	if data, err := securestore.Read(machineKeyItem(), path); err == nil {
		if len(data) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("devicekeys: %s has %d bytes, want %d", path, len(data), ed25519.PrivateKeySize)
		}
		priv := ed25519.PrivateKey(data)
		return &MachineKey{Public: priv.Public().(ed25519.PublicKey), Private: priv}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("devicekeys: read %s: %w", path, err)
	}
	k, err := NewMachineKey()
	if err != nil {
		return nil, err
	}
	if err := securestore.Write(machineKeyItem(), path, k.Private); err != nil {
		return nil, fmt.Errorf("devicekeys: write %s: %w", path, err)
	}
	return k, nil
}

func (m *MachineKey) PublicBase64() string   { return base64.StdEncoding.EncodeToString(m.Public) }
func (m *MachineKey) Sign(msg []byte) []byte { return ed25519.Sign(m.Private, msg) }

// ---- Node Key (X25519, WireGuard-style) ----

// NodeKey is the WireGuard-compatible X25519 keypair used for data plane
// encryption. The private scalar follows WireGuard clamping rules so the
// raw bytes can be passed to wireguard-go without re-derivation.
type NodeKey struct {
	Public  [32]byte
	Private [32]byte
}

// NewNodeKey generates a fresh X25519 keypair with WireGuard clamping.
func NewNodeKey() (*NodeKey, error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return nil, fmt.Errorf("devicekeys: x25519 rand: %w", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pubBytes, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("devicekeys: x25519 derive: %w", err)
	}
	var pub [32]byte
	copy(pub[:], pubBytes)
	return &NodeKey{Public: pub, Private: priv}, nil
}

// LoadOrCreateNodeKey reads or generates the node key. Storage format is
// the raw 32-byte private key (clamping is recomputed on save).
func LoadOrCreateNodeKey(path string) (*NodeKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, fmt.Errorf("devicekeys: %s has %d bytes, want 32", path, len(data))
		}
		var priv [32]byte
		copy(priv[:], data)
		// Re-clamp defensively in case the file was hand-edited.
		priv[0] &= 248
		priv[31] &= 127
		priv[31] |= 64
		pubBytes, err := curve25519.X25519(priv[:], curve25519.Basepoint)
		if err != nil {
			return nil, fmt.Errorf("devicekeys: derive: %w", err)
		}
		var pub [32]byte
		copy(pub[:], pubBytes)
		return &NodeKey{Public: pub, Private: priv}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("devicekeys: read %s: %w", path, err)
	}
	k, err := NewNodeKey()
	if err != nil {
		return nil, err
	}
	if err := secrets.WriteSecret(path, k.Private[:]); err != nil {
		return nil, fmt.Errorf("devicekeys: write %s: %w", path, err)
	}
	return k, nil
}

func (n *NodeKey) PublicBase64() string { return base64.StdEncoding.EncodeToString(n.Public[:]) }

// SaveNodeKey writes the node key's raw 32-byte private scalar to path
// with secret protection (0600). Used by the rotation loop to stage a
// freshly generated key (node.key.next) before promoting it (#228).
func SaveNodeKey(path string, k *NodeKey) error {
	if k == nil {
		return fmt.Errorf("devicekeys: SaveNodeKey: nil key")
	}
	if err := secrets.WriteSecret(path, k.Private[:]); err != nil {
		return fmt.Errorf("devicekeys: write %s: %w", path, err)
	}
	return nil
}

// ---- Public-key parsing helpers ----

// DecodeX25519PublicKey accepts std/url base64 of a 32-byte WireGuard
// public key and returns the raw bytes.
func DecodeX25519PublicKey(s string) ([]byte, error) {
	b, err := decodeBase64(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	return b, nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}
