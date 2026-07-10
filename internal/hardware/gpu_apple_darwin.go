//go:build darwin

package hardware

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"time"
)

// detectApple reports Apple Silicon GPUs on macOS as a single entry
// when running on arm64. Intel Macs fall back to no GPU (the
// Auto-Selector catalog ships no Intel-Mac variants). VRAMTotalMB is
// left at 0 on the GPU record — EffectiveVRAMMB() pulls the real
// budget from Profile.UsableVRAMMB which the per-OS defaultUMA hook
// populates from `sysctl iogpu.wired_limit_mb`.
func detectApple(ctx context.Context) ([]GPU, Accelerators, error) {
	if runtime.GOARCH != "arm64" {
		return nil, Accelerators{}, nil
	}
	model := "Apple GPU"
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "system_profiler", "SPDisplaysDataType", "-json").Output()
	if err == nil {
		if name := parseSPDisplaysGPUName(out); name != "" {
			model = name
		}
	}
	return []GPU{{Vendor: "apple", Model: model}}, Accelerators{Metal: true}, nil
}

// parseSPDisplaysGPUName extracts the first GPU model from
// `system_profiler SPDisplaysDataType -json` output. Apple uses
// `sppci_model` (newer macOS) or `_name` (older) as the model key.
func parseSPDisplaysGPUName(out []byte) string {
	var doc struct {
		SPDisplaysDataType []map[string]any `json:"SPDisplaysDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return ""
	}
	for _, d := range doc.SPDisplaysDataType {
		if v, ok := d["sppci_model"].(string); ok && v != "" {
			return v
		}
		if v, ok := d["_name"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
