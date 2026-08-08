package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The install-time host measurement, kept on disk (#496).
//
// Why it lives in the state dir and not in the boot benchmark's cache
// file. benchCacheKey returns "" when there is no GPU model, so that
// cache disables itself on CPU-only hosts — which is precisely the host
// this measurement exists to describe. It would also sit under
// XDG_CACHE_HOME, where losing it is expected; losing this one costs a
// re-measure on the next install path AND leaves `waired inference
// status` unable to say why local inference is off.

// HostSpeedRecord is the on-disk form of the measurement: the figure
// exactly as it goes on the wire, plus whether it is what set the
// local-inference default to off.
type HostSpeedRecord struct {
	// Measurement is the published figure. nil is not written — a record
	// with nothing measured is simply absent.
	//
	// Named Measurement rather than the obvious Speed because
	// scripts/ci/protoconsumer matches producers by bare field NAME across
	// the whole repo: a local field called Speed reads as a producer for
	// hostfit.Presentation.Speed and quietly voids its exemption.
	Measurement *signer.HostSpeed `json:"measurement,omitempty"`

	// TurnedInferenceOff records that THIS measurement is what turned
	// local inference off, so `waired inference status` can say why
	// rather than leaving the operator to guess.
	//
	// It is a claim about causation, so it is cleared by
	// WriteDesiredInferenceState — i.e. by anyone moving the toggle for
	// any other reason. The measurement itself survives that: it is still
	// a true fact about the host and is still published. Only the "and
	// that is why" is dropped.
	TurnedInferenceOff bool `json:"turned_inference_off,omitempty"`

	// AgentVersion is the buildinfo.Version of the daemon that took the
	// figure, and it is half of what decides whether the figure still
	// applies (the other half is the engine build, which travels on the
	// wire as EngineKind/EngineVersion).
	//
	// It is here rather than on the wire because it answers a question
	// only this host asks: "has anything about this install changed since
	// I last measured?" Every install and every upgrade restarts the
	// daemon on a new version, so keying on it is what makes the
	// measurement an install-time step rather than a once-per-machine one
	// (waired#1099). A record from an older agent is re-measured even
	// though the hardware and the engine are identical — deliberately: the
	// thing being measured is what THIS build does on THIS host, and a
	// figure a previous build produced is not evidence about that.
	//
	// Empty means a record written before this field existed. Treated as
	// "does not match", so the first boot after an upgrade re-measures,
	// which is the behaviour the field exists to produce.
	AgentVersion string `json:"agent_version,omitempty"`
}

// HostSpeedPath is the on-disk location of the host measurement.
func HostSpeedPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "host-speed.json")
}

// ReadHostSpeed returns the persisted measurement. A missing file yields
// a zero record and no error: never measured is the ordinary state of a
// host that has not reached an install path yet.
//
// An unparseable file is also a zero record rather than an error. The
// record is advisory — it re-measures — and refusing to boot over a
// corrupt advisory file would be the worse failure.
func ReadHostSpeed(stateDir string) (HostSpeedRecord, error) {
	raw, err := os.ReadFile(HostSpeedPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostSpeedRecord{}, nil
		}
		return HostSpeedRecord{}, err
	}
	var rec HostSpeedRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return HostSpeedRecord{}, nil
	}
	return rec, nil
}

// WriteHostSpeed persists the measurement.
func WriteHostSpeed(stateDir string, rec HostSpeedRecord) error {
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal host speed: %w", err)
	}
	return atomicWrite(HostSpeedPath(stateDir), append(buf, '\n'), 0o644)
}

// clearHostSpeedCutoffFlag drops the "and that is why local inference is
// off" claim, keeping the measurement. Best-effort: a host with no record
// has nothing to clear, and a write failure leaves a stale claim that the
// status command already guards against by only reporting it while the
// toggle actually reads disabled.
func clearHostSpeedCutoffFlag(stateDir string) {
	rec, err := ReadHostSpeed(stateDir)
	if err != nil || !rec.TurnedInferenceOff {
		return
	}
	rec.TurnedInferenceOff = false
	_ = WriteHostSpeed(stateDir, rec)
}
