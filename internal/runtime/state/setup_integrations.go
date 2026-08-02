package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// SetupIntegrations records the coding-tool instruction an elevated setup
// executor reported as DONE on this device (waired-agent#312).
//
// It is the observable source of truth the coding-tools row never had. The
// engine and model rows are re-derived from disk and engine probes on every
// snapshot, so a restarted daemon reports the same truth it did before; the
// integration row was projected purely from in-memory lease state, so every
// service restart walked a finished device back to "the setup command has not
// run here" — and, before the code split, to "needs administrator access".
//
// Only the daemon writes it, and only on an executor's `done`. The write
// itself stays daemon-side deliberately: the files the step creates live in
// the invoking user's home and in root-owned managed settings, which is
// exactly why the daemon must not be the one to create them — but recording
// that somebody else did is not a privilege the daemon lacks.
type SetupIntegrations struct {
	// Targets are the integration ids the executor wrote, sorted and
	// de-duplicated. Never empty on disk: see WriteSetupIntegrations.
	Targets []string `json:"targets"`

	// WrittenAt is when the daemon accepted the report, RFC3339. Kept for
	// diagnostics — nothing reads it back, and no decision may depend on
	// it: this record ages exactly as well as the files it describes,
	// which is to say indefinitely.
	WrittenAt string `json:"written_at,omitempty"`
}

// Covers reports whether this record satisfies the instruction in want.
//
// A superset counts. An instruction that SHRANK — the operator unticked a
// tool in the browser and re-ran setup — leaves what was already written in
// place, so the row is still honestly done for everything now being asked
// for; the removal is `waired unlink`'s job, not this row's.
//
// A zero record covers nothing, including an empty want. The caller asks
// this question to find out whether anyone has ever written the row, and
// "nobody has" must not read as yes.
func (r SetupIntegrations) Covers(want []string) bool {
	if len(r.Targets) == 0 {
		return false
	}
	for _, w := range want {
		if !slices.Contains(r.Targets, w) {
			return false
		}
	}
	return true
}

// SetupIntegrationsPath is the on-disk location of the record. Missing
// file means no executor has ever finished the coding-tools step here.
//
// Not a `desired-*` file: those hold what the operator asked for, this
// holds what actually happened.
func SetupIntegrationsPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "setup-integrations")
}

// ReadSetupIntegrations parses <state-dir>/runtime/setup-integrations. A
// missing file returns the zero record and no error — the ordinary state of
// every device nobody has run the setup command on. Returns an error only
// when the file exists and cannot be parsed.
func ReadSetupIntegrations(stateDir string) (SetupIntegrations, error) {
	body, err := os.ReadFile(SetupIntegrationsPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SetupIntegrations{}, nil
		}
		return SetupIntegrations{}, err
	}
	var r SetupIntegrations
	if err := json.Unmarshal(body, &r); err != nil {
		return SetupIntegrations{}, fmt.Errorf("runtime/state: parse setup-integrations: %w", err)
	}
	r.Targets = normaliseIntegrationTargets(r.Targets)
	return r, nil
}

// WriteSetupIntegrations persists the record, normalising the target list
// so a reordered instruction is not a different record.
//
// A record with no targets is rejected rather than written. "Nothing was
// written" is the absence of the file: an empty list on disk would read
// back as a record that covers nothing, which is the same answer with an
// extra way to be wrong.
func WriteSetupIntegrations(stateDir string, r SetupIntegrations) error {
	r.Targets = normaliseIntegrationTargets(r.Targets)
	if len(r.Targets) == 0 {
		return errors.New("runtime/state: setup-integrations needs at least one target")
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal setup-integrations: %w", err)
	}
	return atomicWrite(SetupIntegrationsPath(stateDir), append(body, '\n'), 0o644)
}

// normaliseIntegrationTargets sorts, de-duplicates and drops empties, so
// Covers is a plain set test and the file is stable across writes.
func normaliseIntegrationTargets(in []string) []string {
	var out []string
	for _, t := range in {
		if t == "" || slices.Contains(out, t) {
			continue
		}
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}
