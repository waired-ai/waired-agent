package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AppliedResidency records the last model-residency instruction this
// device ACTED on (waired-agent#861) — the control plane's ask for how
// long the engine should keep a model in memory after the last request,
// applied through the same controller `waired inference residency` and
// the app's preset rows use.
//
// It exists because the control plane never clears a desired value: it
// re-sends its instruction on every map frame. Without a durable
// "already acted on this value" marker, every daemon restart would
// re-apply a weeks-old instruction over whatever a person has chosen
// locally since — and, worse, the re-apply would land within the poll
// interval of any local change, which would make the CLI and the app's
// controls a lie. With the marker, only a NEW value acts; a later local
// change stands until the control plane actually says something
// different.
//
// This is the SetupInference arrangement (waired-agent#597) for a
// setting that is not part of setup. It is deliberately not named
// setup-*: residency applies to every enrolled device, and folding it
// into the setup records would make a device that has only ever received
// a residency instruction look like one a wizard is driving.
type AppliedResidency struct {
	// Value is the acted-on instruction as a Go duration string, "0s"
	// meaning hold the model indefinitely. Never empty on disk: "no
	// instruction ever acted on" is the absence of the file.
	Value string `json:"value"`

	// AppliedAt is when the daemon applied it, RFC3339. Diagnostics only —
	// no decision may depend on it.
	AppliedAt string `json:"applied_at,omitempty"`
}

// AppliedResidencyPath is the on-disk location of the record. Missing
// file means no control-plane residency instruction has ever been acted
// on here.
func AppliedResidencyPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "applied-residency")
}

// ReadAppliedResidency parses <state-dir>/runtime/applied-residency. A
// missing file returns the zero record and no error. Returns an error
// only when the file exists and cannot be parsed.
func ReadAppliedResidency(stateDir string) (AppliedResidency, error) {
	body, err := os.ReadFile(AppliedResidencyPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AppliedResidency{}, nil
		}
		return AppliedResidency{}, err
	}
	var r AppliedResidency
	if err := json.Unmarshal(body, &r); err != nil {
		return AppliedResidency{}, fmt.Errorf("runtime/state: parse applied-residency: %w", err)
	}
	return r, nil
}

// WriteAppliedResidency persists the record. An empty value is rejected
// rather than written: "never acted" is the absence of the file, and an
// empty value on disk would read back as a record of nothing.
func WriteAppliedResidency(stateDir string, r AppliedResidency) error {
	if r.Value == "" {
		return errors.New("runtime/state: applied-residency needs a value")
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal applied-residency: %w", err)
	}
	return atomicWrite(AppliedResidencyPath(stateDir), append(body, '\n'), 0o644)
}
