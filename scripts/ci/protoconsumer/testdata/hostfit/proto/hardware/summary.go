// Fixture reproducing the #180 shape: the published wire carries the
// host-fit facts, and no producer in this tree ever writes them. The
// real regression looked exactly like this after PR #146 was merged onto
// a non-main base and never reached main.
package hardware

type HardwareSummary struct {
	Hostname      string `json:"hostname"`
	UnifiedMemory bool   `json:"unified_memory,omitempty"`
	UsableVRAMMB  int    `json:"usable_vram_mb,omitempty"`
	GPUs          []GPU  `json:"gpus,omitempty"`

	unexported string
}

type GPU struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model"`
}

// notAStruct must not contribute fields.
type notAStruct = string
