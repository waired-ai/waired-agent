package elevation

import (
	"os"
	"runtime"
	"testing"
)

// TestIsElevated pins the Unix contract (IsElevated == euid 0) and smoke-
// exercises the call on every OS. CI has no elevated runner, so the
// true branch on Windows can't be asserted here — the per-PR Windows
// installtest contract (waired#749) covers the elevated behavior end to
// end. This just guards that the predicate builds and never panics, and
// that on Unix it tracks euid exactly.
func TestIsElevated(t *testing.T) {
	got := IsElevated()
	switch runtime.GOOS {
	case "linux", "darwin":
		if want := os.Geteuid() == 0; got != want {
			t.Errorf("IsElevated() = %v, want %v (euid=%d)", got, want, os.Geteuid())
		}
	default:
		_ = got // smoke only: must build and not panic
	}
}
