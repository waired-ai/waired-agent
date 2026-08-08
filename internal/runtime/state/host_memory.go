package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The install-time available-memory measurement, kept on disk
// (waired-agent#568). Same home and same reuse rule as the host-speed
// record: the state dir survives cache cleanup, and AgentVersion keys
// the reuse so every install/upgrade re-measures while a plain daemon
// restart does not (waired#1099's ruling — once per install, not once
// per machine).
//
// The reading is only trustworthy while no engine or model is resident
// — a resident model would be charged against the very host serving it
// — so the daemon takes it before the engine bootstrap, and keeps the
// previous record when something is already listening on the engine
// port (an operator-run engine the daemon does not manage).

// HostMemoryRecord is the on-disk form of the measurement.
type HostMemoryRecord struct {
	// AvailableGB is the published figure: available system RAM in
	// whole GiB, rounded once at measure time so this record, the wire
	// (signer.HardwareSummary.RAMAvailableGB) and both fit adapters
	// compute on the same integer. A successful probe never stores 0 —
	// the probe floors a near-exhausted reading at 1, because 0 on the
	// wire means "measurement unavailable".
	//
	// Named AvailableGB rather than the wire's RAMAvailableGB because
	// scripts/ci/protoconsumer matches producers by bare field NAME
	// across the repo (the HostSpeedRecord.Measurement trap).
	AvailableGB int `json:"available_gb,omitempty"`

	// MeasuredAt is when the reading was taken (RFC 3339). Diagnostic —
	// `waired inference status` can show what the fit decisions are
	// based on (#589); nothing computes on it.
	MeasuredAt string `json:"measured_at,omitempty"`

	// AgentVersion is the buildinfo.Version of the daemon that
	// measured. Same contract as HostSpeedRecord.AgentVersion: a
	// mismatch (including empty, a record from before this field
	// existed) means re-measure on the next clean boot.
	AgentVersion string `json:"agent_version,omitempty"`
}

// HostMemoryPath is the on-disk location of the measurement.
func HostMemoryPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "host-memory.json")
}

// ReadHostMemory returns the persisted measurement. A missing file
// yields a zero record and no error: never measured is the ordinary
// state of a host that has not booted a daemon since #568. An
// unparseable file is also a zero record rather than an error — the
// record is advisory (it re-measures), and refusing to boot over a
// corrupt advisory file would be the worse failure.
func ReadHostMemory(stateDir string) (HostMemoryRecord, error) {
	raw, err := os.ReadFile(HostMemoryPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostMemoryRecord{}, nil
		}
		return HostMemoryRecord{}, err
	}
	var rec HostMemoryRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return HostMemoryRecord{}, nil
	}
	return rec, nil
}

// WriteHostMemory persists the measurement.
func WriteHostMemory(stateDir string, rec HostMemoryRecord) error {
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal host memory: %w", err)
	}
	return atomicWrite(HostMemoryPath(stateDir), append(buf, '\n'), 0o644)
}
