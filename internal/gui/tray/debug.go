package tray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DebugEnvVar is the environment variable that turns on the
// "dump menu state to JSON" facility used by the Phase W-3 Windows
// port iteration loop. When set to any non-empty value, every call
// to apply(MenuModel) writes a snapshot of the rendered intent to
// $TEMP/waired-tray-debug.json so a screenshot-based iteration can
// cross-check what the menu *should* be showing against what
// systray actually rendered.
//
// The file is small (~1 KB), atomically replaced on each write
// (best-effort: failures are silent), and lives outside any state
// directory so it is trivial to clean up.
const DebugEnvVar = "WAIRED_TRAY_DEBUG"

// debugPath is the absolute path of the on-disk snapshot. The path
// is computed once (per process) and cached because TempDir() reads
// $TEMP / TMP env each call and we want a single stable file.
var (
	debugPathOnce sync.Once
	debugPathStr  string
)

func debugPath() string {
	debugPathOnce.Do(func() {
		debugPathStr = filepath.Join(os.TempDir(), "waired-tray-debug.json")
	})
	return debugPathStr
}

// iconName turns the unexported IconState enum into a human-readable
// label for the debug dump. Centralised here so the dump matches what
// the screenshot loop expects to see.
func iconName(s IconState) string {
	switch s {
	case IconConnected:
		return "connected"
	case IconDisconnected:
		return "disconnected"
	case IconError:
		return "error"
	case IconDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// debugSnapshot is the JSON shape written to disk. It deliberately
// flattens MenuModel into a single object (instead of nesting it
// under "model") so a human reading the file can scan the rendered
// state without an extra level of indentation. Timestamp is RFC3339
// nano so a screenshot script can correlate the file mtime with the
// `apply` call that produced it.
type debugSnapshot struct {
	Timestamp string    `json:"ts"`
	Icon      string    `json:"icon"`
	Model     MenuModel `json:"model"`
}

// dumpDebugState writes m as JSON to $TEMP/waired-tray-debug.json
// when WAIRED_TRAY_DEBUG is set. Best-effort: any error is swallowed
// (the debug dump is advisory; the tray must keep running). Safe to
// call concurrently — the write is atomic via tempfile+rename, so
// concurrent readers (the screenshot loop) never see a half-written
// file.
func dumpDebugState(m MenuModel) {
	if os.Getenv(DebugEnvVar) == "" {
		return
	}
	snap := debugSnapshot{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Icon:      iconName(m.Icon),
		Model:     m,
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	path := debugPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
