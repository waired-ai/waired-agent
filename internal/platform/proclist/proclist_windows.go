//go:build windows

package proclist

import (
	"context"
	"os/exec"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/pwsh"
)

// list enumerates processes via PowerShell's CIM cmdlet, which returns each
// process's full CommandLine directly — the modern replacement for the
// deprecated wmic the repo avoids (internal/hardware/profiler_windows.go),
// and safer than a PEB walk (no ReadProcessMemory). This runs once per
// tuning verification, not on any hot path.
//
// ExecutablePath rides the same query, so Windows needs no second call to
// answer what darwin needs a second `ps` for: the program path, apart from
// the command-line string. Ollama spawns the runner unquoted, so an install
// under a path with a space — %AppData%\waired, when the profile name has
// one — would otherwise shatter argv[0] exactly as it did on macOS
// (waired-agent#1303). A null ExecutablePath decodes to "" and the parse
// falls back to splitting the command line alone.
func list() ([]ProcInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// -NoProfile/-NonInteractive keep it fast and headless; ConvertTo-Json
	// with a large -Depth avoids truncating the record set.
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,CommandLine,ExecutablePath | ConvertTo-Json -Depth 3 -Compress")
	// Windows PowerShell 5.1 must not inherit a PowerShell 7 PSModulePath
	// (#178) — see internal/platform/pwsh.
	cmd.Env = pwsh.Env()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCimJSON(out)
}
