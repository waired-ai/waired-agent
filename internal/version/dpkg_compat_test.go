package version

import (
	"os/exec"
	"testing"
)

// TestOrderingMatchesDpkg pins the ordering against dpkg itself over
// every pair of a realistic version set.
//
// This is a product contract, not a record of today's behaviour: on
// Linux the "latest" side of every update comparison is an apt
// candidate, and apt picked it with dpkg's ordering. A comparator that
// disagrees offers the host a version apt will refuse to install, or
// hides one it would (waired-agent#780, waired-agent#781; the campaign
// evidence is waired-ai/waired#1217).
//
// The versions are in Debian spelling because that is what dpkg accepts;
// TestCompare covers the SemVer spelling and pins that the two agree.
// Skipped where dpkg is absent — the Linux legs build in a Debian
// container, so it runs there.
func TestOrderingMatchesDpkg(t *testing.T) {
	if _, err := exec.LookPath("dpkg"); err != nil {
		t.Skip("dpkg not available on this host")
	}
	versions := []string{
		"0.0.1~rc2", "0.0.1~rc9", "0.0.1~rc10", "0.0.1~rc18",
		"0.0.2~rc1", "0.0.2~rc8", "0.0.2~rc8~dev", "0.0.2~rc9", "0.0.2",
		"0.0.3~rc1", "0.0.3~rc2", "0.0.3~rc10", "0.0.3",
		"0.0.3~edge.20260610143000", "0.0.3~edge.20260610143001",
		"1.0.0~rc1", "1.0.0", "1.2.3", "10.0.0",
	}
	for _, a := range versions {
		for _, b := range versions {
			got, ok := Compare(a, b)
			if !ok {
				t.Fatalf("Compare(%q, %q): unparseable", a, b)
			}
			want := 0
			switch {
			case dpkgSays(t, a, "lt", b):
				want = -1
			case dpkgSays(t, a, "gt", b):
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%q, %q) = %d, dpkg says %d", a, b, got, want)
			}
		}
	}
}

func dpkgSays(t *testing.T, a, op, b string) bool {
	t.Helper()
	return exec.Command("dpkg", "--compare-versions", a, op, b).Run() == nil
}
