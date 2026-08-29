package main

import (
	"fmt"
	"regexp"
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

// checkHostClass rejects a missing host, or one outside the declared
// vocabulary. Required rather than optional: every record in both stores
// already names one, and a record that cannot say what ran it cannot be
// compared with the next one.
func checkHostClass(cmd, v string) error {
	if v == "" {
		return fmt.Errorf("%s: --host is required with --import; it names a hardware CLASS, "+
			"never an identifier — one of: %s", cmd, catalog.HostClassList())
	}
	if !catalog.ValidHostClass(v) {
		return fmt.Errorf("%s: --host %q is not a known hardware class (one of: %s). "+
			"If this is a new class, add it to catalog.HostClasses in the same PR — "+
			"this repository is public, and a machine name must never reach the store",
			cmd, v, catalog.HostClassList())
	}
	return nil
}
