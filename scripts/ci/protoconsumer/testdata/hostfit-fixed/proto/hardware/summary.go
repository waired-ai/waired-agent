// Same wire as testdata/hostfit, with the producer re-landed.
package hardware

type HardwareSummary struct {
	Hostname      string `json:"hostname"`
	UnifiedMemory bool   `json:"unified_memory,omitempty"`
	UsableVRAMMB  int    `json:"usable_vram_mb,omitempty"`
	GPUs          []GPU  `json:"gpus,omitempty"`
}

type GPU struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model"`
}
