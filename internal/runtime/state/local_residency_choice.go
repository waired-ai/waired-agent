package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LocalResidencyChoice records when a person AT THIS MACHINE last set
// model residency — through `waired inference residency` or the app's
// "Keep model in memory" rows (waired#1232).
//
// It is the ordering half of the control plane's realignment, and it is a
// timestamp rather than a flag because the useful claim is an ORDER. The
// instruction is sticky: the control plane folds it into every signed map
// for as long as it is set, and agent-side convergence is
// act-once-per-value — which is what lets a local change stick, and also
// what leaves the instruction describing the device wrongly forever
// afterwards. "The person here chose AFTER that instruction was written"
// is what licenses moving the instruction; "a person chose at some point"
// is not, because it cannot tell a local override from a device that has
// simply not finished applying a change an operator made in the browser a
// moment ago.
//
// Written ONLY for a choice made here. A value the desired-state
// reconciler applied is the instruction arriving, not an answer to it,
// and recording it would let an instruction confirm itself — the control
// plane would read its own echo as local intent and realign onto the
// value it had just sent.
//
// The sibling of AppliedResidency, and deliberately a separate file: that
// one records what the CONTROL PLANE asked and this one records what a
// PERSON HERE chose, they are written on different paths, and a single
// record would have to answer "who" on every read. Same arrangement as
// LocalModelChoiceAt, which reads provenance off the preference file
// rather than off the applied-state record.
type LocalResidencyChoice struct {
	// ChosenAt is when the choice was made, RFC3339Nano. This is the
	// field the wire carries and the only one any decision may read.
	ChosenAt string `json:"chosen_at"`

	// Value is what was chosen, as a Go duration string. Diagnostics only
	// — the residency actually in force is read from the live engine, and
	// a consumer that took this instead would report a value the engine
	// may have moved on from.
	Value string `json:"value,omitempty"`
}

// LocalResidencyChoicePath is the on-disk location of the record. A
// missing file means nobody has set residency on this machine since the
// record existed, which every consumer must read as "no ordering
// available" rather than as "nobody ever has".
func LocalResidencyChoicePath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "local-residency-choice")
}

// ReadLocalResidencyChoice parses
// <state-dir>/runtime/local-residency-choice. A missing file returns the
// zero record and no error. Returns an error only when the file exists
// and cannot be parsed.
func ReadLocalResidencyChoice(stateDir string) (LocalResidencyChoice, error) {
	body, err := os.ReadFile(LocalResidencyChoicePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LocalResidencyChoice{}, nil
		}
		return LocalResidencyChoice{}, err
	}
	var r LocalResidencyChoice
	if err := json.Unmarshal(body, &r); err != nil {
		return LocalResidencyChoice{}, fmt.Errorf("runtime/state: parse local-residency-choice: %w", err)
	}
	return r, nil
}

// WriteLocalResidencyChoice persists the record. An empty timestamp is
// rejected rather than written: "nobody has chosen here" is the absence
// of the file, and an empty instant on disk would read back as a record
// that answers nothing while looking like one that answers something.
func WriteLocalResidencyChoice(stateDir string, r LocalResidencyChoice) error {
	if r.ChosenAt == "" {
		return errors.New("runtime/state: local-residency-choice needs a timestamp")
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal local-residency-choice: %w", err)
	}
	return atomicWrite(LocalResidencyChoicePath(stateDir), append(body, '\n'), 0o644)
}
