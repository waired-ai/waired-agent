//go:build linux || darwin

package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestInstanceMgmtEndpoint_LongPathStaysBindable keeps the unix socket within
// sockaddr_un.sun_path (108 on Linux, 104 on darwin); past it bind() fails.
// Unix-only: a Windows pipe name has no such limit.
func TestInstanceMgmtEndpoint_LongPathStaysBindable(t *testing.T) {
	deep := filepath.Join(filepath.FromSlash("/tmp"), strings.Repeat("verylongsegment/", 12), "state")
	got := InstanceMgmtEndpoint(deep)
	if got == "" {
		t.Fatal("deep non-default state dir produced no instance endpoint")
	}
	if len(got) > sunPathBudget {
		t.Fatalf("instance endpoint %q is %d bytes, over the %d-byte sun_path budget", got, len(got), sunPathBudget)
	}
}
