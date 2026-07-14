//go:build windows

package proclist

import (
	"context"
	"os/exec"
	"time"
)

// list enumerates processes via PowerShell's CIM cmdlet, which returns each
// process's full CommandLine directly — the modern replacement for the
// deprecated wmic the repo avoids (internal/hardware/profiler_windows.go),
// and safer than a PEB walk (no ReadProcessMemory). This runs once per
// tuning verification, not on any hot path.
func list() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// -NoProfile/-NonInteractive keep it fast and headless; ConvertTo-Json
	// with a large -Depth avoids truncating the record set.
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_Process | Select-Object ProcessId,CommandLine | ConvertTo-Json -Depth 3 -Compress")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCimJSON(out)
}
