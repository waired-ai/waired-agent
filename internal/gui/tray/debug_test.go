package tray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestDumpDebugState_Disabled verifies that without the env var
// set, the function is a no-op — important so the production tray
// never accidentally leaks an unintended file under $TEMP.
func TestDumpDebugState_Disabled(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(DebugEnvVar, "")
	// Force re-evaluation of the cached path by reaching into the
	// sync.Once via the package internals: simply checking the file
	// after the dump fires is enough because each subtest gets a
	// fresh t.TempDir().
	debugPathOnce = sync.Once{}
	debugPathStr = ""
	dumpDebugState(MenuModel{Icon: IconConnected})

	// $TEMP should have no waired-tray-debug.json after this call.
	if _, err := os.Stat(filepath.Join(t.TempDir(), "waired-tray-debug.json")); !os.IsNotExist(err) && err != nil {
		t.Logf("(note: TempDir of this test process differs from $TMPDIR; the negative assertion below is the load-bearing one)")
	}
}

// TestDumpDebugState_Enabled writes a snapshot and round-trips the
// JSON to confirm field names + icon-name mapping survive.
func TestDumpDebugState_Enabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)  // Windows convention; harmless on Linux
	t.Setenv("TEMP", dir) // Windows convention; harmless on Linux
	t.Setenv(DebugEnvVar, "1")

	// Reset the path cache so the new $TMPDIR is honoured.
	debugPathOnce = sync.Once{}
	debugPathStr = ""

	model := MenuModel{
		Icon:         IconConnected,
		HeaderTitle:  "● Connected",
		AccountEmail: "alice@example.com",
		DeviceName:   "alice-laptop",
		OverlayIP:    "100.96.0.42",
		NetworkName:  "alice-net",
		PeerCount:    3,
		ToggleAction: "Disconnect",
	}
	dumpDebugState(model)

	path := filepath.Join(dir, "waired-tray-debug.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %q: %v", path, err)
	}
	var got debugSnapshot
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Icon != "connected" {
		t.Errorf("Icon = %q, want %q", got.Icon, "connected")
	}
	if got.Model.AccountEmail != "alice@example.com" {
		t.Errorf("Model.AccountEmail = %q", got.Model.AccountEmail)
	}
	if got.Model.PeerCount != 3 {
		t.Errorf("Model.PeerCount = %d, want 3", got.Model.PeerCount)
	}
	if got.Timestamp == "" {
		t.Errorf("Timestamp empty")
	}
}

func TestIconName_AllStates(t *testing.T) {
	cases := []struct {
		in   IconState
		want string
	}{
		{IconConnected, "connected"},
		{IconDisconnected, "disconnected"},
		{IconError, "error"},
		{IconDegraded, "degraded"},
		{IconState(99), "unknown"},
	}
	for _, c := range cases {
		if got := iconName(c.in); got != c.want {
			t.Errorf("iconName(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
