// Package version provides dependency-free comparison of dotted-numeric
// version strings (X.Y.Z). It tolerates a leading "v", a leading
// "ollama version is " style prefix, and trailing suffixes like "-rc1"
// or ".post1" — enough for comparing waired's own build version and a
// reused Ollama's reported version without pulling in a full semver
// library.
//
// It is the shared primitive behind:
//   - the bundled-Ollama floor check (internal/runtime/ollama_version.go,
//     a thin back-compat shim over AtLeast)
//   - the installer-driven update check (#292) and, later, the
//     `waired update` resolver (#293) and background auto-check (#294),
//     which compare the installed build against the latest release.
package version

import (
	"strconv"
	"strings"
)

// Valid reports whether s parses as a dotted-numeric version (with the
// same prefix/suffix tolerance as AtLeast/Compare). Used by manifest
// validation to reject garbage in version-floor fields at load time.
func Valid(s string) bool {
	_, ok := parseDotted(s)
	return ok
}

// AtLeast reports whether version v (e.g. "0.24.0", or the raw
// "ollama version is 0.24.0" line) is >= min. Unparseable input returns
// false (treated as "not known-good").
//
// Note: a shorter v is considered older than a longer min once their
// shared prefix matches — AtLeast("1.2", "1.2.0") == false. This matches
// the historical OllamaVersionAtLeast behaviour and is intentionally
// preserved; use Compare for zero-padded equality semantics.
func AtLeast(v, min string) bool {
	av, ok1 := parseDotted(v)
	mv, ok2 := parseDotted(min)
	if !ok1 || !ok2 {
		return false
	}
	for i := range mv {
		if i >= len(av) {
			return false
		}
		if av[i] != mv[i] {
			return av[i] > mv[i]
		}
	}
	return true
}

// Compare returns -1, 0, or +1 for a<b, a==b, a>b, comparing dotted
// components left-to-right and zero-padding the shorter operand
// (so "1.2" == "1.2.0"). ok is false when either input is unparseable;
// callers decide how to treat that (the installer treats "unknown" as
// "offer the update").
func Compare(a, b string) (int, bool) {
	av, ok1 := parseDotted(a)
	bv, ok2 := parseDotted(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	n := max(len(av), len(bv))
	for i := range n {
		x, y := 0, 0
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseDotted extracts the leading numeric dotted components from s,
// tolerating prefixes like "ollama version is " and suffixes like
// "-rc1" / ".post1".
func parseDotted(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, " "); idx >= 0 {
		s = s[idx+1:] // drop "ollama version is " style prefix
	}
	s = strings.TrimPrefix(s, "v")
	// Cut at the first non [0-9.] char so "-rc1"/".post1" don't break us.
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			s = s[:i]
			break
		}
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
