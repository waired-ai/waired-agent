package catalog

import (
	"fmt"
	"sort"
	"time"
)

// MigrationOpts customises how MigrateInPlace fills in fields the
// older schema did not record.
type MigrationOpts struct {
	// Now is the wall-clock the migrated record stamps as decided_at.
	// Defaults to time.Now() when zero.
	Now func() time.Time

	// Hardware is the snapshot embedded into ActiveSelection.
	// DecisionHardwareSnapshot when migration synthesises an Active
	// from v1's existing model entries.
	Hardware HardwareSnapshot
}

// MigrationReport tells the caller what the migration did. The agent
// bootstrap uses this to decide whether to persist the migrated state
// back to disk and whether to surface a notification ("state.json
// migrated v1 → v2; consider running waired runtimes refresh").
type MigrationReport struct {
	// Migrated is true iff the on-disk state was older than current
	// StateVersion and was rewritten.
	Migrated bool

	// FromVersion is the version the file was originally at. 0 means
	// the file existed but had no version field; treated like v1.
	FromVersion int

	// ToVersion is the version the State now claims to be at (always
	// equals StateVersion after MigrateInPlace returns).
	ToVersion int

	// PreservedActive describes the v1 → v2 carry-over: which model_id
	// became Active, derived from the most-recently-used ready entry
	// in the v1 Models map. Empty when no ready model existed.
	PreservedActive string

	// Notes collects any human-readable warnings (e.g. "no ready model
	// found in v1; Active left unset"). Surface these via the bootstrap
	// log / foreground prompt.
	Notes []string
}

// MigrateInPlace upgrades st to current StateVersion, mutating in
// place. Idempotent: a v2 state passes through unchanged with
// MigrationReport.Migrated=false.
//
// v1 → v2 strategy (compat-first per Q13 #i):
//
//   - Preserve the most-recently-used Ready model from st.Models as
//     ActiveSelection, with DecidedBy="migration". This means a Phase A
//     user with qwen3-8b-instruct already pulled keeps using it; Step 2's
//     "Update available" notification is what nudges them toward a
//     potentially better pick on their hardware.
//   - Initialise Runtimes with an inferred ollama entry when at least
//     one Ready model exists (we know ollama was running on Phase A).
//     vLLM entries appear only after `waired runtimes install vllm`.
//   - ExternalManifests stays empty (Step 3+ feature).
func MigrateInPlace(st *State, opts MigrationOpts) MigrationReport {
	if st == nil {
		return MigrationReport{}
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	report := MigrationReport{
		FromVersion: st.Version,
		ToVersion:   StateVersion,
	}

	if st.Version >= StateVersion {
		// Even on a current-version load, normalise nil maps so callers
		// never have to nil-check before iterating.
		ensureMaps(st)
		return report
	}

	// v0 / v1 → v2.
	report.Migrated = true
	st.Version = StateVersion
	ensureMaps(st)

	if st.Active == nil {
		picked := pickV1Active(st.Models)
		if picked != "" {
			ms := st.Models[picked]
			st.Active = &ActiveSelection{
				Runtime:                  RuntimeOllama,
				ModelID:                  picked,
				VariantID:                ms.VariantID,
				DecidedAt:                now(),
				DecidedBy:                "migration",
				DecisionReason:           []string{fmt.Sprintf("preserved from Phase A state.json (model %q)", picked)},
				DecisionHardwareSnapshot: opts.Hardware,
			}
			report.PreservedActive = picked
		} else {
			report.Notes = append(report.Notes,
				"no ready model found in v1 state; ActiveSelection left unset (run `waired runtimes install --auto`)")
		}
	}

	if len(st.Runtimes) == 0 && report.PreservedActive != "" {
		st.Runtimes = map[string]RuntimeInstall{
			RuntimeOllama: {
				// Phase A didn't track ollama version in state, so we
				// leave it empty; bootstrap fills it from the live
				// `ollama --version` query the next time it runs.
				InstalledAt: now(),
			},
		}
	}

	return report
}

// pickV1Active selects the model_id in models that should become the
// migrated ActiveSelection. Strategy: most-recently-used Ready entry,
// breaking ties by alphabetic model_id for determinism. Returns ""
// when no Ready entry exists.
func pickV1Active(models map[string]ModelState) string {
	type candidate struct {
		id   string
		used time.Time
	}
	var cands []candidate
	for id, ms := range models {
		if ms.State != ModelStateReady {
			continue
		}
		cands = append(cands, candidate{id: id, used: ms.LastUsedAt})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].used.Equal(cands[j].used) {
			return cands[i].used.After(cands[j].used)
		}
		return cands[i].id < cands[j].id
	})
	return cands[0].id
}

// ensureMaps is a small helper so the rest of the agent can iterate
// st.Models / st.Endpoints / st.Runtimes without nil-checking.
func ensureMaps(st *State) {
	if st.Models == nil {
		st.Models = map[string]ModelState{}
	}
	if st.Endpoints == nil {
		st.Endpoints = map[string]EndpointState{}
	}
	if st.Runtimes == nil {
		st.Runtimes = map[string]RuntimeInstall{}
	}
}
