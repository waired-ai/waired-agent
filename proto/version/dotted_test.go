package version

import "testing"

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"0.24.0", "0.6.0", true},
		{"ollama version is 0.24.0", "0.6.0", true},
		{"0.6.0", "0.6.0", true},
		{"0.5.9", "0.6.0", false},
		{"v0.7.1-rc1", "0.6.0", true},
		{"0.6.3.post1", "0.6.0", true},
		{"garbage", "0.6.0", false},
		{"0.6.0", "garbage", false},
		// Historical quirk: shorter v is "older" than a longer min once
		// the shared prefix matches. Preserved for back-compat.
		{"1.2", "1.2.0", false},
		{"1.2.0", "1.2", true},
		// PRODUCT CONTRACT (owner ruling 2026-08-21, waired-agent#804): an
		// engine floor ignores the prerelease, so a prerelease of the floor
		// version clears it. Floors are authored against released engines,
		// and a host on a prerelease of one has opted in; refusing it would
		// withhold the model with a message about a version the operator
		// visibly has.
		//
		// The catalog depends on this being settled, not merely current:
		// the agent's decision 20260816/2024-qwen3-8-takes-the-27b-band.md
		// picks the ollama pin 0.32.13 and records that 0.32.14-rc0 was NOT
		// taken, because a prerelease authored as a floor would silently
		// mean its release. Changing this changes what that catalog entry
		// means.
		//
		// Compare deliberately disagrees, and both are right for their own
		// callers — see AtLeast's doc comment.
		//
		// (These two rows called this "a record of today's behaviour, NOT a
		// product contract" and sent the reader to #804 as the open
		// question until waired-agent#1260. #804 had been closed by the
		// ruling above since 2026-08-20; dotted.go's own doc was corrected
		// under waired-agent#970 and this file was not.)
		{"0.6.0-rc1", "0.6.0", true},
		{"0.6.0~rc1", "0.6.0", true},
		// The floor side too, which is the direction the decision above
		// actually relies on: a prerelease written as a floor is the
		// release it leads to.
		{"0.32.13", "0.32.14-rc0", false},
		{"0.32.14", "0.32.14-rc0", true},
	}
	for _, c := range cases {
		if got := AtLeast(c.v, c.min); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		name   string
		a, b   string
		want   int
		wantOK bool
	}{
		{"equal", "1.2.3", "1.2.3", 0, true},
		{"patch older", "1.2.3", "1.2.4", -1, true},
		{"patch newer", "1.2.4", "1.2.3", 1, true},
		{"v-prefix tolerated", "v1.2.3", "1.2.3", 0, true},
		{"zero-padded equality", "1.2", "1.2.0", 0, true},
		{"zero-padded equality, reversed", "1.2.0", "1.2", 0, true},
		{"minor beats patch", "2.0", "1.9.9", 1, true},
		{"last token taken", "ollama version is 0.6.0", "0.6.0", 0, true},
		{"unparseable a", "garbage", "1.0.0", 0, false},
		{"unparseable b", "1.0.0", "", 0, false},
		{"the moving edge tag is not a version", "0.0.3", "edge", 0, false},

		// waired-agent#781. This inverts the previous pin
		// (`{"0.0.1-rc6", "0.0.1-rc7", 0, true}` — "suffix dropped →
		// equal"), which is the defect: every rc-to-rc update compared
		// equal and was reported as "already up to date".
		{"rc to rc", "0.0.1-rc6", "0.0.1-rc7", -1, true},
		{"prerelease is below its release", "0.0.3-rc1", "0.0.3", -1, true},
		{"release is above its prerelease", "0.0.3", "0.0.3-rc1", 1, true},
		{"prerelease is above the previous release", "0.0.3-rc1", "0.0.2", 1, true},

		// The numeric run is why comparePre is not SemVer §11's lexical
		// rule for alphanumeric identifiers: that would order rc10 BELOW
		// rc2. This repository has already shipped an rc18.
		{"rc2 below rc10", "0.0.1-rc2", "0.0.1-rc10", -1, true},
		{"rc10 above rc9", "0.0.1-rc10", "0.0.1-rc9", 1, true},
		{"rc18 above rc2", "0.0.1-rc18", "0.0.1-rc2", 1, true},

		// The two spellings of one release must compare equal: on Linux
		// the running build's SemVer string is compared against an apt
		// candidate, which is in Debian spelling (waired-agent#780).
		{"tilde and hyphen are one release", "0.0.3-rc1", "0.0.3~rc1", 0, true},
		{"tilde form orders like the hyphen form", "0.0.3~rc1", "0.0.3~rc2", -1, true},
		{"tilde prerelease is below its release", "0.0.3~rc1", "0.0.3", -1, true},
		{"tilde prerelease is above the previous release", "0.0.3~rc1", "0.0.2-rc9", 1, true},

		// The multi-hyphen preview tag that outranked the next rc under
		// dpkg's last-hyphen split (waired-agent#780). Here it is one
		// prerelease string, ordered by its digit run.
		{"rc8-dev below rc9", "0.0.2-rc8-dev", "0.0.2-rc9", -1, true},
		{"deb spelling of the same pair", "0.0.2~rc8~dev", "0.0.2~rc9", -1, true},
		// The deliberate deviation from SemVer §11, which would put a
		// prerelease with more identifiers ABOVE one with fewer. dpkg
		// sorts "0.0.2~rc8~dev" below "0.0.2~rc8", that is the ordering
		// apt applies to the .deb, and the two spellings of one build
		// have to agree. See comparePre.
		{"rc8-dev below rc8", "0.0.2-rc8-dev", "0.0.2-rc8", -1, true},
		{"deb spelling of that pair too", "0.0.2~rc8~dev", "0.0.2~rc8", -1, true},
		// A dotted identifier is unaffected: it sorts above the bare one
		// under both rules.
		{"dotted identifier is above the bare prerelease", "0.0.2-rc8.dev", "0.0.2-rc8", 1, true},

		// A Debian epoch carries ordering information dpkg needs and this
		// package does not: nothing here emits one, and dropping it keeps
		// an epoch-bearing apt candidate comparable with the SemVer build.
		{"epoch dropped", "1:0.0.3~rc1", "0.0.3-rc1", 0, true},

		// SemVer §10: build metadata is not part of precedence.
		{"build metadata ignored", "0.0.3-edge.20260610143000+abc1234", "0.0.3-edge.20260610143000+def5678", 0, true},
		{"edge builds order by timestamp", "0.0.3-edge.20260610143000+abc1234", "0.0.3-edge.20260610143001+abc1234", -1, true},
		{"edge is below the release it is based on", "0.0.3~edge.20260610143000+abc1234", "0.0.3", -1, true},
		{"an rc is above an edge of the same core", "0.0.3-rc1", "0.0.3-edge.20260610143000", 1, true},

		// ".post1" is not introduced by a separator, so it is not a
		// prerelease — it must not sort below the release.
		{"post-release tail is not a prerelease", "0.6.3.post1", "0.6.3", 0, true},

		{"dev sentinel", "0.0.0-dev", "0.0.1", -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Compare(c.a, c.b)
			if ok != c.wantOK || (ok && got != c.want) {
				t.Errorf("Compare(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, got, ok, c.want, c.wantOK)
			}
			// Antisymmetry: whatever the rule, reversing the operands
			// must reverse the sign. A one-directional bug here reads as
			// "update available" in one place and "up to date" in another.
			rev, revOK := Compare(c.b, c.a)
			if revOK != ok || (ok && rev != -got) {
				t.Errorf("Compare(%q, %q) = (%d, %v); reversed = (%d, %v), want (%d, %v)",
					c.a, c.b, got, ok, rev, revOK, -got, ok)
			}
		})
	}
}

func TestValid(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"1.2.3", true},
		{"0.0.3-rc1", true},
		{"0.0.3~rc1", true},
		{"v0.0.3", true},
		{"", false},
		{"edge", false},
		{"latest", false},
	}
	for _, c := range cases {
		if got := Valid(c.s); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
