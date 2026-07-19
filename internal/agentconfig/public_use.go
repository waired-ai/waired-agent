package agentconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// PublicUseFileName is the on-disk filename for the consumer-side
// Public Share settings + consent record. Colocated with the other
// inference state (same trust boundary as preferred-model.json).
const PublicUseFileName = "public_use.json"

// Public Share consumer modes. The zero value ("" — treated as off)
// and PublicUseModeOff both mean public candidates never appear in
// routing; the distinction is only whether the user ever touched the
// setting.
const (
	PublicUseModeOff      = "off"
	PublicUseModeAuto     = "auto"     // public candidates only when they beat the best own-node tier
	PublicUseModeExplicit = "explicit" // public candidates unconditionally (tier/class filters still apply)
)

// PublicUse is the JSON shape of public_use.json — the consumer-side
// Public Share settings and the versioned consent record. Local file,
// no secrets; the loopback management API is the only writer.
//
// Enforcement note: with Consent == nil the router must treat Mode as
// off regardless of the stored value (use EffectiveMode) — and
// enforcement is always router-side candidate filtering, never a
// gateway 4xx.
type PublicUse struct {
	// Mode is off / auto / explicit. Empty means off (never consented
	// or never enabled).
	Mode string `json:"mode,omitempty"`
	// MinQualityTier is the lower bound on a public node's advertised
	// model tier. 0 = no threshold.
	MinQualityTier int `json:"min_quality_tier,omitempty"`
	// Main / Sub toggle whether main-class and sub-class requests may
	// use public candidates.
	Main bool `json:"main"`
	Sub  bool `json:"sub"`
	// Consent is the accepted warning record; nil = never consented.
	Consent *PublicConsent `json:"consent,omitempty"`
}

// PublicConsent records acceptance of the Public Share warning text.
// WarningVersion pins which text was accepted — when the served
// warning's version moves past it, consumers must re-consent before
// Mode can be anything but off.
type PublicConsent struct {
	AcceptedAt     time.Time `json:"accepted_at"`
	WarningVersion int       `json:"warning_version"`
}

// Consented reports whether a consent record exists for exactly
// warningVersion (a stale version requires re-consent).
func (p PublicUse) Consented(warningVersion int) bool {
	return p.Consent != nil && p.Consent.WarningVersion == warningVersion
}

// EffectiveMode is the mode the router must enforce: off until a
// consent record for the current warning version exists, the stored
// mode afterwards. Spec §4.2 — no consent ⇒ no public candidates ever.
func (p PublicUse) EffectiveMode(warningVersion int) string {
	if !p.Consented(warningVersion) {
		return PublicUseModeOff
	}
	if p.Mode == "" {
		return PublicUseModeOff
	}
	return p.Mode
}

// ValidPublicUseMode reports whether m is a settable mode value.
func ValidPublicUseMode(m string) bool {
	switch m {
	case PublicUseModeOff, PublicUseModeAuto, PublicUseModeExplicit:
		return true
	}
	return false
}

// DefaultPublicUsePath returns the on-disk location of public_use.json
// under <StateDir>/inference/.
func DefaultPublicUsePath() string {
	return filepath.Join(paths.StateDir(paths.AutoDetect), "inference", PublicUseFileName)
}

// LoadPublicUse returns the persisted settings, or (zero, false, nil)
// when the file does not exist. A malformed file or an invalid stored
// mode is an error so the operator notices rather than silently
// falling back.
func LoadPublicUse(path string) (PublicUse, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PublicUse{}, false, nil
		}
		return PublicUse{}, false, fmt.Errorf("public-use: read %s: %w", path, err)
	}
	var p PublicUse
	if err := json.Unmarshal(data, &p); err != nil {
		return PublicUse{}, false, fmt.Errorf("public-use: parse %s: %w", path, err)
	}
	if p.Mode != "" && !ValidPublicUseMode(p.Mode) {
		return PublicUse{}, false, fmt.Errorf("public-use: %s: invalid mode %q", path, p.Mode)
	}
	return p, true, nil
}

// SavePublicUse writes p atomically with the same protection posture
// as the other inference state files (0600 on Unix, DACL on Windows).
func SavePublicUse(path string, p PublicUse) error {
	if p.Mode != "" && !ValidPublicUseMode(p.Mode) {
		return fmt.Errorf("public-use: invalid mode %q", p.Mode)
	}
	if err := secrets.SecureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("public-use: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("public-use: marshal: %w", err)
	}
	if err := secrets.WriteSecret(path, data); err != nil {
		return fmt.Errorf("public-use: write: %w", err)
	}
	return nil
}
