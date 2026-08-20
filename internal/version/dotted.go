// Package version provides dependency-free comparison of the version
// strings this project emits and reads. It tolerates a leading "v", an
// "ollama version is " style prefix, a Debian epoch, and SemVer build
// metadata, and it orders prereleases ("-rc1") below the release they
// lead to — enough for comparing waired's own build version against a
// release tag or an apt candidate, and an engine's reported version
// against a catalog floor, without pulling in a full semver library.
//
// It is the shared primitive behind:
//   - the catalog's MinEngineVersion floors (manifest validation)
//   - the installer-driven update check (#292), the `waired update`
//     resolver (#293) and background auto-check (#294), which compare the
//     installed build against the latest release.
//
// packaging/install/install.sh (version_lt) and
// packaging/install/install.ps1 (Compare-WairedVersion) implement the
// same ordering, so the installer, `waired update` and the auto-check
// agree on "is X older than Y".
//
// # Version shapes
//
// One release is spelled two ways. The Go build and the release tag use
// SemVer's hyphen ("0.0.3-rc1"); the .deb Version field uses Debian's
// tilde ("0.0.3~rc1"), because dpkg sorts "~" below everything and would
// otherwise sort the prerelease ABOVE its release
// (waired-agent#780). Both spellings normalize to the same value here —
// the Linux update path compares the running build's SemVer string
// against an apt candidate in Debian spelling, so anything else reports
// a permanent false "update available".
package version

import (
	"strings"
)

// Valid reports whether s parses as a version string (with the same
// prefix/suffix tolerance as AtLeast/Compare). Used by manifest
// validation to reject garbage in version-floor fields at load time.
func Valid(s string) bool {
	_, ok := parse(s)
	return ok
}

// AtLeast reports whether version v (e.g. "0.24.0", or the raw
// "ollama version is 0.24.0" line) is >= min. Unparseable input returns
// false (treated as "not known-good").
//
// AtLeast compares the dotted-numeric part ONLY: "0.6.0-rc1" is at least
// "0.6.0". That is the contract, settled by the owner on 2026-08-21
// (waired-agent#804): an ENGINE FLOOR ignores the prerelease on both
// sides.
//
// The floors are authored against released engines — a catalog entry
// naming 0.30.0 is naming the release — and a host running a prerelease
// of that version is a developer who opted into it. Refusing them makes
// the model unavailable with a message about a version they visibly have,
// which is a worse answer than admitting an engine that is, by its own
// version number, the thing the floor asked for.
//
// The catalog already relies on this, which is what makes it a contract
// rather than an accident:
// docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md picks 0.32.13
// as the ollama pin and records WHY 0.32.14-rc0 was not taken — because
// AtLeast is prerelease-blind, so authoring a prerelease as a floor would
// silently mean its release.
//
// So this package deliberately carries two orderings, and they are not in
// competition. Compare answers "which of these two builds is newer",
// where a prerelease must sort below its release or every rc-to-rc update
// reports "already up to date" (waired-agent#781). AtLeast answers "does
// this engine clear a floor", where the prerelease is noise on a number
// authored against the release. Use Compare for the first question and
// this for the second; neither is the other's fallback.
//
// Note also that a shorter v is considered older than a longer min once
// their shared prefix matches — AtLeast("1.2", "1.2.0") == false. Use
// Compare for zero-padded equality semantics.
func AtLeast(v, min string) bool {
	av, ok1 := parse(v)
	mv, ok2 := parse(min)
	if !ok1 || !ok2 {
		return false
	}
	for i := range mv.core {
		if i >= len(av.core) {
			return false
		}
		if av.core[i] != mv.core[i] {
			return av.core[i] > mv.core[i]
		}
	}
	return true
}

// Compare returns -1, 0, or +1 for a<b, a==b, a>b. Dotted components are
// compared left-to-right with the shorter operand zero-padded (so "1.2"
// == "1.2.0"); when those are equal, a prerelease sorts below the
// release it leads to ("0.0.3-rc1" < "0.0.3") and two prereleases are
// ordered by comparePre. ok is false when either input is unparseable;
// callers decide how to treat that (the installer treats "unknown" as
// "offer the update").
func Compare(a, b string) (int, bool) {
	av, ok1 := parse(a)
	bv, ok2 := parse(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	n := max(len(av.core), len(bv.core))
	for i := range n {
		x, y := 0, 0
		if i < len(av.core) {
			x = av.core[i]
		}
		if i < len(bv.core) {
			y = bv.core[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case av.pre == "" && bv.pre == "":
		return 0, true
	case av.pre == "": // a is the release, b a prerelease of it
		return 1, true
	case bv.pre == "":
		return -1, true
	}
	return comparePre(av.pre, bv.pre), true
}

// version is a parsed version string: the dotted-numeric release
// components, plus the prerelease text after the first separator ("" for
// a release).
type version struct {
	core []int
	pre  string
}

// parse normalizes s and splits it into release core and prerelease.
//
// Normalization drops, in order: an "ollama version is " style prefix, a
// Debian epoch ("1:"), a leading "v", and SemVer build metadata
// ("+abc1234", not part of precedence — SemVer §10). It then rewrites
// Debian's "~" prerelease separator to SemVer's "-", which is what makes
// the .deb and SemVer spellings of one release compare equal.
func parse(s string) (version, bool) {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, " "); idx >= 0 {
		s = s[idx+1:] // drop "ollama version is " style prefix
	}
	if i := strings.IndexByte(s, ':'); i > 0 && isDigits(s[:i]) {
		s = s[i+1:] // Debian epoch
	}
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "~", "-")

	core, pre, _ := strings.Cut(s, "-")
	// The core tolerates a trailing non-numeric tail that is NOT introduced
	// by a separator — ".post1" and friends. Cutting there rather than
	// failing is long-standing engine-floor behaviour, and that tail is not
	// a prerelease: "0.6.3.post1" is a post-release, above 0.6.3, and
	// treating it as a prerelease would put it below.
	for i, r := range core {
		if (r < '0' || r > '9') && r != '.' {
			core = core[:i]
			break
		}
	}
	out := version{pre: pre}
	for _, p := range strings.Split(core, ".") {
		if p == "" {
			continue
		}
		n, ok := atoi(p)
		if !ok {
			return version{}, false
		}
		out.core = append(out.core, n)
	}
	if len(out.core) == 0 {
		return version{}, false
	}
	return out, true
}

// comparePre orders two prerelease strings with dpkg's algorithm: the
// string is read as alternating runs of non-digits and digits; digit
// runs compare numerically, non-digit runs compare character by
// character under the ordering in preRank.
//
// It is dpkg's and not SemVer §11's for two reasons, both of which the
// SemVer rule gets wrong for the versions this project ships:
//
//   - SemVer compares alphanumeric identifiers lexically, which puts
//     "rc10" BELOW "rc2". This repository has already shipped an rc18.
//   - On Linux the value on the other side of the comparison is an apt
//     candidate, chosen by dpkg's ordering. A comparator that disagrees
//     with it reports a "latest" the host cannot actually install.
//
// The one deliberate deviation from SemVer: a second separator sorts
// BELOW the empty string, so "rc8-dev" < "rc8" where SemVer would say
// the opposite. That follows from the two constraints above — the .deb
// spelling of that build is "0.0.2~rc8~dev", dpkg sorts it below
// "0.0.2~rc8", and the two spellings of one build have to agree. Dotted
// identifiers are unaffected: "rc8.dev" > "rc8" under both rules.
func comparePre(a, b string) int {
	for len(a) > 0 || len(b) > 0 {
		ra, rb := runOf(a, false), runOf(b, false)
		if c := compareRunes(a[:ra], b[:rb]); c != 0 {
			return c
		}
		a, b = a[ra:], b[rb:]

		da, db := runOf(a, true), runOf(b, true)
		if c := compareDigits(a[:da], b[:db]); c != 0 {
			return c
		}
		a, b = a[da:], b[db:]
	}
	return 0
}

// compareRunes compares two non-digit runs character by character under
// preRank, with the end of a run treated as its own (middle) rank.
func compareRunes(a, b string) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0 // 0 is preRank's value for "the run ended here"
		if i < len(a) {
			x = preRank(a[i])
		}
		if i < len(b) {
			y = preRank(b[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// preRank is dpkg's modified character ordering for the non-digit runs
// of a version: a separator sorts before anything including the end of
// the run, then the end of the run, then letters, then everything else.
// dpkg spells the separator "~"; the SemVer form of the same version
// spells it "-", and parse has already rewritten one to the other.
func preRank(c byte) int {
	switch {
	case c == '-' || c == '~':
		return -1
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return int(c)
	default:
		return int(c) + 256
	}
}

// runOf returns the length of the leading run of digits (digits=true) or
// non-digits (digits=false) in s.
func runOf(s string, digits bool) int {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') == digits {
		i++
	}
	return i
}

// compareDigits orders two runs of decimal digits by value, without
// parsing them into an integer: an edge build's timestamp run
// ("20260610143000") is already 14 digits and there is no reason for the
// comparison to have a width limit. Leading zeros are insignificant, so
// a longer run is a larger number, and equal-length runs compare
// byte-wise. An absent run ("") is zero.
func compareDigits(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// atoi parses a run of decimal digits. Returns ok=false for anything
// else, including an empty string or a signed/spaced number.
func atoi(s string) (int, bool) {
	if s == "" || !isDigits(s) {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return 0, false // absurd component; treat as unparseable
		}
	}
	return n, true
}

// isDigits reports whether s is a non-empty run of decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
