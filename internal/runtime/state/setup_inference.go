package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SetupInference records the last DesiredInference instruction this device
// ACTED on (waired-agent#597) — the NAVI wizard's explicit "don't run
// local AI on this computer" / "turn it back on" answer, applied as the
// same persisted soft toggle a person's own `waired inference off|on`
// writes.
//
// It exists for the same reason the toggle itself is persisted (#465): the
// control plane never clears a desired value and re-sends it on every map
// frame, so without a durable "already acted on this value" marker every
// daemon restart would re-apply a weeks-old wizard answer over whatever a
// person has chosen locally since — the exact silent revert #465 forbids.
// With the marker, only a NEW value acts; a person's later local flip
// stands until the wizard actually says something different.
type SetupInference struct {
	// Value is the acted-on instruction: signer.DesiredInferenceOn or
	// signer.DesiredInferenceOff. Never empty on disk: "no instruction
	// ever acted on" is the absence of the file.
	Value string `json:"value"`

	// AppliedAt is when the daemon applied it, RFC3339. Diagnostics only —
	// no decision may depend on it.
	AppliedAt string `json:"applied_at,omitempty"`
}

// SetupInferencePath is the on-disk location of the record. Missing file
// means no wizard local-AI instruction has ever been acted on here.
//
// Not a `desired-*` file: those hold what the operator asked for, this
// holds what actually happened (the SetupIntegrations rationale).
func SetupInferencePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "setup-inference")
}

// ReadSetupInference parses <state-dir>/runtime/setup-inference. A missing
// file returns the zero record and no error. Returns an error only when
// the file exists and cannot be parsed.
func ReadSetupInference(stateDir string) (SetupInference, error) {
	body, err := os.ReadFile(SetupInferencePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SetupInference{}, nil
		}
		return SetupInference{}, err
	}
	var r SetupInference
	if err := json.Unmarshal(body, &r); err != nil {
		return SetupInference{}, fmt.Errorf("runtime/state: parse setup-inference: %w", err)
	}
	return r, nil
}

// WriteSetupInference persists the record. An empty value is rejected
// rather than written: "never acted" is the absence of the file, and an
// empty value on disk would read back as a record of nothing.
func WriteSetupInference(stateDir string, r SetupInference) error {
	if r.Value == "" {
		return errors.New("runtime/state: setup-inference needs a value")
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal setup-inference: %w", err)
	}
	return atomicWrite(SetupInferencePath(stateDir), append(body, '\n'), 0o644)
}
