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

// PreferenceFileName is the on-disk filename used by Get/SavePreference.
// Lives next to inference/state.json so that anyone with access to one
// has access to the other (same trust boundary).
const PreferenceFileName = "preferred-model.json"

// Preference is the JSON shape of preferred-model.json. The agent and
// the loopback management API are the only writers; the file is
// considered local and contains no secrets.
type Preference struct {
	ModelID string    `json:"model_id"`
	SetAt   time.Time `json:"set_at,omitempty"`

	// None records that the operator chose to run WITHOUT a local model
	// (install-flow "don't download a model now", waired-agent#586;
	// owner-ruled 2026-08-08, waired-ai/waired#1067). Mutually exclusive
	// with ModelID: a later model choice overwrites the whole file. It is
	// a choice, not an error state — the engine stays installed and a
	// model can be added any time — and its one effect at boot is that
	// the bundled fallback pre-pull stands down.
	None bool `json:"none,omitempty"`

	// Unanswered records that the install flow — the terminal picker or
	// the browser wizard — asked which model to download and the question
	// expired with no answer (waired-agent#586; owner ruling 2026-08-09,
	// recorded on that issue). An abandoned question is not consent: the
	// bundled fallback download stays down on EVERY boot until an actual
	// answer (a model, or None) replaces this record — without the
	// persistence, the next daemon restart would fold the host back into
	// the never-asked arm and start the multi-GB download the operator
	// never agreed to. Hosts that were never asked (non-interactive and
	// fleet installs, ordinary restarts) never carry this record and keep
	// the spec §11.1 auto-pull.
	Unanswered bool `json:"unanswered,omitempty"`

	// Source records who put this record here: PreferenceSourceOperator
	// for an answer given by a person at this machine (the install
	// picker, the slow-host demotion prompt, the tray), or
	// PreferenceSourceDesired for the setup reconciler applying an
	// instruction that arrived from the control plane.
	//
	// The file could not previously tell the two apart — both wrote a
	// bare ModelID — and two separate defects came out of that. The
	// install picker skips itself when a preference exists, so an
	// instruction arriving mid-install silently removed a step a person
	// was supposed to see (waired-agent#627). And the control plane has
	// no way to learn that a person here chose something other than what
	// it asked for, which is why a demoted host keeps being told to run
	// the model the wizard itself rejected (waired-agent#647).
	//
	// Empty means UNKNOWN, not "operator": a file written before this
	// field existed carries no answer either way, and both consumers
	// above must keep doing what they did before rather than assume.
	Source string `json:"source,omitempty"`
}

// Accepted values for Preference.Source. Empty — "unknown, written
// before the field existed" — is deliberately not a constant: it is the
// field's zero value, not a value anyone writes.
const (
	// PreferenceSourceOperator: a person at this machine answered.
	PreferenceSourceOperator = "operator"
	// PreferenceSourceDesired: the setup reconciler applied a
	// control-plane instruction.
	PreferenceSourceDesired = "desired"
)

// ChosenHere reports whether this record is an answer a person at this
// machine gave — a model, or the explicit "run without a local model".
//
// An Unanswered record is not one: it means the question was put and
// nobody replied, which is the case its own field exists to describe.
func (p Preference) ChosenHere() bool {
	if p.Source != PreferenceSourceOperator {
		return false
	}
	return p.ModelID != "" || p.None
}

// DefaultPreferencePath returns the on-disk location of preferred-model.json,
// colocated with inference state.json under <StateDir>/inference/. Prior
// to the platform/paths consolidation this lived under $XDG_STATE_HOME
// on Linux — operators upgrading across that boundary need to migrate
// the file by hand (the agent will simply re-create an empty
// preference if the new path is missing).
func DefaultPreferencePath() string {
	return filepath.Join(paths.StateDir(paths.AutoDetect), "inference", PreferenceFileName)
}

// LoadPreference returns the persisted user preference, or ("", false, nil)
// when the file does not exist. A malformed file is reported as an error
// rather than silently ignored so the operator notices.
func LoadPreference(path string) (Preference, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Preference{}, false, nil
		}
		return Preference{}, false, fmt.Errorf("preference: read %s: %w", path, err)
	}
	var p Preference
	if err := json.Unmarshal(data, &p); err != nil {
		return Preference{}, false, fmt.Errorf("preference: parse %s: %w", path, err)
	}
	if p.ModelID == "" && !p.None && !p.Unanswered {
		// Treat a present-but-empty file as "no preference" rather than
		// pinning the agent to the empty string. A None record is NOT
		// empty: "run without a model" is a stated choice (#586), and
		// reporting it as absence is how a boot would re-arm the fallback
		// download the choice exists to stand down. An Unanswered record
		// is not empty either, for the mirrored reason: reporting it as
		// absence is how a restart would turn an abandoned question into
		// consent.
		return Preference{}, false, nil
	}
	return p, true, nil
}

// SavePreference writes p atomically with the same protection posture
// as the inference state.json (Secret on Unix → 0600, Windows DACL on
// Windows). The parent directory is created if needed.
func SavePreference(path string, p Preference) error {
	if p.SetAt.IsZero() {
		p.SetAt = time.Now().UTC()
	}
	if err := secrets.SecureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("preference: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("preference: marshal: %w", err)
	}
	if err := secrets.WriteSecret(path, data); err != nil {
		return fmt.Errorf("preference: write: %w", err)
	}
	return nil
}

// ApplyPreferenceOverride merges a persisted preference into c, taking
// precedence over the existing PreferredModelID when both are set. This
// runs after the regular defaults → JSON → env → flags chain so the
// tray-driven choice survives across restarts without forcing the
// operator to thread a CLI flag through their service unit.
//
// A None preference (#586) deliberately changes nothing here: it names no
// model, and the config it would want to influence — the bundled
// fallback — is stood down by the provider reading Preference.None
// directly, not by clearing BundledModelID (which status surfaces and
// `isBundledModel` still need to answer questions about).
func ApplyPreferenceOverride(c *InferenceConfig, p Preference) {
	if p.ModelID == "" {
		return
	}
	c.PreferredModelID = p.ModelID
}
