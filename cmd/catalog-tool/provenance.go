package main

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// The operator-supplied half of a measurement record, checked identically
// for both stores.
//
// Almost everything a record holds is derived from the report: the grade,
// the trials, the transport, the harness revision, the engine build. Three
// fields cannot be, because nothing in the artifact knows them — which
// hardware class ran it, which CI run produced it, and when. What is
// possible is refusing the values a fabricated or careless record would
// have to get wrong, and refusing them the SAME WAY on both sides.
//
// That last part is the defect these functions close (waired-agent#1117).
// shapes checked the run URL and agentgrade did not; neither checked the
// date beyond emptiness, so "2026-8-030" was a legal measurement date in
// both; neither checked the host at all. The result is visible in the
// shipped stores: one run produced a shape record carrying its Actions
// URL and a verdict record carrying none.

// runURLPattern is checked when a run URL is supplied. It is not
// required: the documented way to measure is `make e2e-agentgrade` on a
// GPU host, which has no Actions run to point at, and requiring one
// would make the honest local path unusable while a fabricated URL
// stays trivial to type.
var runURLPattern = regexp.MustCompile(`^https://github\.com/waired-ai/waired-agent/actions/runs/\d+`)

// retrievedLayout is the measurement date's layout. Parsed rather than
// pattern-matched: a regexp for the SHAPE accepts 2026-13-45, and the
// emptiness check both importers shipped with accepted 2026-8-030.
const retrievedLayout = "2006-01-02"

// checkRunURL rejects a supplied run URL that is not an Actions run for
// this repository. An empty one is allowed — see runURLPattern.
func checkRunURL(cmd, v string) error {
	if v == "" || runURLPattern.MatchString(v) {
		return nil
	}
	return fmt.Errorf("%s: --run-url %q is not an Actions run for this repository", cmd, v)
}

// checkRetrieved rejects a missing or unparseable measurement date.
func checkRetrieved(cmd, v string) error {
	if v == "" {
		return fmt.Errorf("%s: --retrieved YYYY-MM-DD is required with --import "+
			"(a record with no date cannot be aged out)", cmd)
	}
	if _, err := time.Parse(retrievedLayout, v); err != nil {
		return fmt.Errorf("%s: --retrieved %q is not a date in YYYY-MM-DD form", cmd, v)
	}
	return nil
}

// checkHostClass rejects a missing host, or one that is neither a
// well-formed derived key nor a frozen legacy spelling. Required rather
// than optional: every record in both stores already names one, and a
// record that cannot say what ran it cannot be compared with the next
// one.
//
// A legacy spelling passes here but not everywhere: see
// checkLegacyContinuesStore, which is where "frozen" is actually
// enforced. The split is deliberate — whether a spelling is well formed
// is a property of the string, and whether it may START a store is a
// property of the store.
func checkHostClass(cmd, v string) error {
	if v == "" {
		return fmt.Errorf("%s: --host is required with --import; it names a hardware CLASS, "+
			"never an identifier — %s", cmd, catalog.HostClassList())
	}
	if !catalog.ValidHostClass(v) {
		return fmt.Errorf("%s: --host %q is not a well-formed host key. It must be "+
			"<topology>-<vendor>-<chip>, all lowercase, where topology is one of "+
			"unified / discrete / cpu — e.g. unified-amd-ryzen-ai-max-395. Nothing "+
			"should be typing this: it is derived from the measuring host's own "+
			"hardware profile, which is what keeps a machine name out of a public "+
			"repository", cmd, v)
	}
	return nil
}

// checkLegacyContinuesStore allows one of the frozen legacy spellings
// only where the store already carries it.
//
// The three legacy names describe measurements taken before keys were
// derived. Continuing a store under the name it already has is not a new
// claim — it is the same machine, still measuring. Starting a new one
// under a legacy name would be: it would assert a provenance nobody
// re-measured, and it would keep the roster alive by use after it was
// frozen by declaration (waired-agent#1455).
//
// The shipped stores are NOT re-spelled to derived keys, and
// deliberately. Both keys are in fact known now — the reference host
// reads unified-amd-ryzen-ai-max-395 off the machine, and the GPU lane
// is "a g2-standard-4 with one L4" (installtest-inference.yml), an L4
// reporting compute capability 8.9, so discrete-nvidia-sm89. Knowing
// them is not a reason to rewrite them: waired-agent#1117 settled that
// shipped records stay as they were measured, and a bulk re-spelling
// would edit provenance without taking a measurement. The reader
// accepts both spellings; the lane writes its own key the next time it
// imports.
func checkLegacyContinuesStore(cmd, v string, present []string) error {
	if !catalog.IsLegacyHostClass(v) {
		return nil
	}
	if slices.Contains(present, v) {
		return nil
	}
	return fmt.Errorf("%s: --host %q is a frozen legacy spelling and this store does not "+
		"carry it, so the import would start a new one under a name that was retired. "+
		"A new measurement names a DERIVED key — <topology>-<vendor>-<chip>, taken from "+
		"the measuring host's own hardware profile (waired-agent#1455)", cmd, v)
}

// hostsIn collects the host classes a per-record store already carries,
// for checkLegacyContinuesStore. Order and duplicates do not matter:
// the caller only asks whether a spelling is present.
func hostsIn(seq func(yield func(host string) bool)) []string {
	var out []string
	seq(func(h string) bool {
		if h != "" {
			out = append(out, h)
		}
		return true
	})
	return out
}
