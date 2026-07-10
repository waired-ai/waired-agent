package signer

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// DeviceCertificate is the signed document a Waired CP issues to each
// enrolled device. The shape mirrors docs/specs/waired_control_plane_auth_spec.md
// §4.4, reduced to the fields step3 minimum core needs (no auth_epoch /
// revocation_epoch / device_tags yet).
type DeviceCertificate struct {
	Version          int      `json:"version"`
	NetworkID        string   `json:"network_id"`
	DeviceID         string   `json:"device_id"`
	AccountID        string   `json:"account_id"`
	MachinePublicKey string   `json:"machine_public_key"`
	NodePublicKey    string   `json:"node_public_key"`
	OverlayIP        string   `json:"overlay_ip"`
	AllowedServices  []string `json:"allowed_services"`
	IssuedAt         string   `json:"issued_at"`
	ExpiresAt        string   `json:"expires_at"`
	Signature        string   `json:"signature,omitempty"`
}

// SignDeviceCertificate canonicalises cert (with signature blanked),
// signs the bytes with k, and returns a copy with the base64 signature
// populated.
func (k *Key) SignDeviceCertificate(cert DeviceCertificate) (DeviceCertificate, error) {
	cert.Signature = ""
	msg, err := CanonicalJSON(cert)
	if err != nil {
		return DeviceCertificate{}, err
	}
	cert.Signature = base64.StdEncoding.EncodeToString(k.Sign(msg))
	return cert, nil
}

// VerifyDeviceCertificate checks the certificate's embedded signature
// against pub. Returns nil on success.
func VerifyDeviceCertificate(pub ed25519.PublicKey, cert DeviceCertificate) error {
	if cert.Signature == "" {
		return errors.New("cert: missing signature")
	}
	sig, err := base64.StdEncoding.DecodeString(cert.Signature)
	if err != nil {
		return fmt.Errorf("cert: signature base64: %w", err)
	}
	cert.Signature = ""
	msg, err := CanonicalJSON(cert)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("cert: signature does not verify")
	}
	return nil
}

// IssueDefaultDuration is the lifetime baked into a fresh certificate.
// Step3 minimum: 180 days, matching the spec's standard Node Key lifetime.
const IssueDefaultDuration = 180 * 24 * time.Hour

// Node Key rotation lifecycle constants (auth spec §13.1, issue #228).
// Shared by the control plane (rotate endpoint / store) and the agent
// (rotation loop) — this is the one package both sides import.
const (
	// NodeKeyLifetime is the default validity of a Node Key. A network
	// may override it (Network.node_key_lifetime_seconds); when unset the
	// CP falls back to this constant.
	NodeKeyLifetime = 180 * 24 * time.Hour

	// NodeKeyRotationLead is how far before expiry the agent proactively
	// rotates (so with the default lifetime, scheduled rotation lands at
	// ~150 days). Keeping a lead well inside the lifetime means a rotation
	// can fail and be retried many times before the hard expiry.
	NodeKeyRotationLead = 30 * 24 * time.Hour

	// NodeKeyGracePeriod is how long the previous Node Key stays valid
	// after a rotation so in-flight peers that still hold the old key in
	// their last network map keep decrypting until they pick up the new one.
	NodeKeyGracePeriod = 30 * time.Minute

	// NodeKeyForcedRenewalGrace is how long past the hard expiry the agent
	// keeps trying to rotate before it surfaces reauth_required locally
	// (auth spec §4.3 forced_renewal_grace).
	NodeKeyForcedRenewalGrace = 30 * time.Minute
)
