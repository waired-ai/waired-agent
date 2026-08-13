//go:build darwin

package hardware

import (
	"context"
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
//
// Reporting the device on architecture alone is deliberate and stays:
// every Apple Silicon part has an integrated GPU, so arm64 IS the
// driver-level fact here, and there is no probe that could be more
// authoritative than the architecture. What was missing is the other
// half of VendorDetector's contract — a name this could not read used
// to come back as no error at all (waired-agent#35). appleGPUModel
// supplies the warning; the exec stays here and the decision does not.
func detectApple(ctx context.Context) ([]GPU, Accelerators, error) {
	if runtime.GOARCH != "arm64" {
		return nil, Accelerators{}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "system_profiler", "SPDisplaysDataType", "-json").Output()
	model, warn := appleGPUModel(out, err)
	return []GPU{{Vendor: "apple", Model: model}}, Accelerators{Metal: true}, warn
}
