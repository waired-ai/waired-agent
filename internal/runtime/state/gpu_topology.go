package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The GPU topology reading, kept on disk because the daemon cannot take
// it (waired-agent#459, #1455).
//
// WHAT IT IS FOR. internal/hardware asks, per accelerator, whether its
// memory and the operating system's RAM are one physical pool. On Linux
// the answer is a first-class kernel fact — AMDGPU_IDS_FLAGS_FUSION,
// read through DRM_IOCTL_AMDGPU_INFO — and it is behind a device node
// the product cannot open: /dev/dri/renderD* is mode 0660 root:render,
// and the unit runs as User=waired with no supplementary groups. So the
// daemon reads "unknown" forever, which is safe but never improves.
//
// WHY PERSIST RATHER THAN GRANT THE GROUP. The answer never changes for
// a given part, so a standing privilege would buy nothing a single read
// cannot. `sudo waired init` is already the elevated path, already the
// documented way to re-run setup, and already writes state that
// service_linux.go's FixStateOwnership chowns back to the service user.
// A re-setup re-takes the reading, which is also how a swapped GPU stops
// being described by a stale one.
//
// SAME SHAPE AS host-memory.json, and for the same reason: a reading
// that can only be taken under conditions the daemon cannot arrange
// belongs on disk, taken once, with enough beside it to know when it
// stopped applying.

// GPUTopologyRecord is the on-disk form of the reading.
type GPUTopologyRecord struct {
	// Devices holds one entry per accelerator the reading covered.
	// Absent devices are not "discrete": they are unread, which is what
	// the consumer's three-state merge is for.
	Devices []GPUTopologyDevice `json:"devices,omitempty"`

	// MeasuredAt is when the reading was taken (RFC 3339). Diagnostic;
	// nothing computes on it.
	MeasuredAt string `json:"measured_at,omitempty"`

	// AgentVersion is the buildinfo.Version of the CLI that read it.
	// Diagnostic here rather than a reuse key, unlike
	// HostMemoryRecord.AgentVersion: that figure is a MEASUREMENT whose
	// accuracy depends on the build that took it, and this one is a bit
	// the kernel reports. A newer build reads the same bit.
	AgentVersion string `json:"agent_version,omitempty"`
}

// GPUTopologyDevice is one accelerator's reading.
type GPUTopologyDevice struct {
	// PCIID is the accelerator's PCI vendor:device pair, lowercase hex
	// ("1002:1586"). It is the KEY: a consumer applies an entry only to
	// a device reporting the same pair, so replacing the card leaves the
	// old entry matching nothing and the host falls back to unknown
	// rather than to a description of hardware that is gone.
	//
	// A device whose pair could not be read is not recorded at all —
	// there would be nothing to match it by.
	PCIID string `json:"pci_id"`

	// Integrated is the reading: the accelerator's memory and the
	// operating system's RAM are one physical pool.
	//
	// Both values are meaningful. An entry saying false is a POSITIVE
	// finding that the part is discrete, which is why the record holds
	// entries rather than only a list of integrated pairs.
	Integrated bool `json:"integrated"`
}

// GPUTopologyPath is the on-disk location of the reading.
func GPUTopologyPath(stateDir string) string {
	return filepath.Join(stateDir, "runtime", "gpu-topology.json")
}

// ReadGPUTopology returns the persisted reading. A missing file yields a
// zero record and no error: never read is the ordinary state of a host
// that has not run an elevated setup since this landed. An unparseable
// file is also a zero record rather than an error — the record is
// advisory, and refusing to boot over a corrupt advisory file would be
// the worse failure (same rule as ReadHostMemory).
func ReadGPUTopology(stateDir string) (GPUTopologyRecord, error) {
	raw, err := os.ReadFile(GPUTopologyPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GPUTopologyRecord{}, nil
		}
		return GPUTopologyRecord{}, err
	}
	var rec GPUTopologyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return GPUTopologyRecord{}, nil
	}
	return rec, nil
}

// WriteGPUTopology persists the reading.
func WriteGPUTopology(stateDir string, rec GPUTopologyRecord) error {
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/state: marshal gpu topology: %w", err)
	}
	return atomicWrite(GPUTopologyPath(stateDir), append(buf, '\n'), 0o644)
}

// IntegratedFor reports what the record says about the device with this
// PCI pair. ok is false for an empty pair, an unread device, or a record
// that was never written — all of which mean "no reading", never
// "discrete".
func (r GPUTopologyRecord) IntegratedFor(pciID string) (integrated, ok bool) {
	if pciID == "" {
		return false, false
	}
	for _, d := range r.Devices {
		if d.PCIID == pciID {
			return d.Integrated, true
		}
	}
	return false, false
}
