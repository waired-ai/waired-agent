package integration

// This file is deliberately UNTAGGED (the rest of the package is
// `//go:build integration`). The sentinel's two failure-prone decisions —
// how long a leg may retry, and whether a non-2xx can ever become a 2xx —
// are pure functions here, so ci.yml's always-on unit lane compiles and
// tests them on every PR. waired-agent#29 was a silent arithmetic bug in
// exactly this logic that no lane could see: 4 legs x 3min of retries
// exceeded go test's 10min default, so the run was killed before the
// harness could name the HTTP status it had been given.

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/proxy/intercept"
)

// headerFallback is the response header the Claude intercept sets on the real
// ResponseWriter BEFORE replaying upstream, so it survives even the 502 that
// an unreachable upstream produces. It is the ONLY wire evidence of the local
// error the proxy discarded, and taking it from the intercept package (rather
// than re-typing the literal) means a rename cannot silently downgrade the
// classifier to "retry" and re-hide waired-agent#29.
const headerFallback = intercept.HeaderFallback

// reasonAnthropicUnreachable is the X-Waired-Fallback reason the intercept
// sets when an anthropic-routed turn could not reach the real API and was
// served locally instead (#665). It is the mirror image of the local_*
// reasons localStatusFromFallback parses: it says nothing failed locally —
// it says the turn was ROUTED UPSTREAM and the upstream was not there.
//
// On a lane that blackholes api.anthropic.com it is the only thing on the
// wire separating "this turn was served locally because that is its route"
// from "this turn left local routing and was dragged back". Both arrive as
// a local 2xx and both are recorded decision=local, which is why the
// sentinel passed straight through waired-agent#1091 (waired-agent#1141).
const reasonAnthropicUnreachable = "anthropic_unreachable"

// degradedFromAnthropic reports whether the response says this turn was
// routed to the real Anthropic API and only served locally because the
// upstream could not be reached.
func degradedFromAnthropic(hdr http.Header) bool {
	_, reason, found := strings.Cut(hdr.Get(headerFallback), "reason=")
	return found && strings.TrimSpace(reason) == reasonAnthropicUnreachable
}

// Outcome is what a leg CLAIMS about its turn, and what the run records as
// having become of it. The claim is part of the leg table so that a ruling
// which changes where a turn belongs shows up as a diff on the leg rather
// than as a nightly going red weeks later (waired-agent#1141).
type Outcome string

const (
	// outcomeRan: the leg started and did not reach its assertion. On a
	// clean run nothing ends here.
	outcomeRan Outcome = "ran"
	// outcomeLocal: the daemon served the turn on this device — the claim
	// the sentinel was built to make (#496).
	outcomeLocal Outcome = "local"
	// outcomeUpstream: the turn was routed to the real Anthropic API.
	// Naming a model the real API serves is naming where it runs, so this
	// is a correct outcome for the leg that drives one (waired-agent#1091),
	// not a fail-open.
	outcomeUpstream Outcome = "upstream"
)

const (
	// maxLegDriveBudget caps how long ONE leg retries a non-2xx. Two minutes
	// covers a cold 0.5B load plus first prefill on the slowest lane we run;
	// a terminal engine error now ends in seconds instead of consuming it.
	maxLegDriveBudget = 2 * time.Minute
	// legOverhead is the non-drive time a leg still needs after its last
	// attempt: Configure, the ring cursor, the served-locally sentinel, and
	// the failure-path ring read.
	legOverhead = 45 * time.Second
	// runReserve stays unspent so the test binary reports its OWN failure
	// instead of being killed by -timeout. That kill is the regression this
	// file exists to make unrepresentable.
	runReserve = 60 * time.Second
	// driveAttemptTimeout bounds ONE request. Without it a single attempt
	// could burn the HTTP client's whole timeout and overshoot the leg budget.
	driveAttemptTimeout = 45 * time.Second
	// sentinelTimeout is how long the event ring is polled for the
	// served-locally proof after a 2xx drive.
	sentinelTimeout = 30 * time.Second
	// driveRetryDelay is the pause between drive attempts.
	driveRetryDelay = 2 * time.Second
	// progressEvery is how often a slow attempt reports that it is still
	// waiting, so a hang is visible WHILE it hangs.
	progressEvery = 15 * time.Second
)

// legDriveBudget divides the time the test binary has LEFT evenly among the
// legs still to run and subtracts each leg's non-drive overhead. Deriving the
// budget from -timeout makes "the legs cannot outlive the test binary"
// structural rather than arithmetic a reviewer has to redo.
//
// until <= 0 means no deadline (go test -timeout 0); the cap is then the only
// bound. A budget of 0 is a legal answer — the caller still makes ONE attempt,
// so a failure always carries a real status.
func legDriveBudget(until time.Duration, remainingLegs int) time.Duration {
	if remainingLegs <= 0 {
		return 0
	}
	if until <= 0 {
		return maxLegDriveBudget
	}
	per := (until-runReserve)/time.Duration(remainingLegs) - legOverhead
	return max(min(per, maxLegDriveBudget), 0)
}

// driveVerdict is what to do about one drive response.
type driveVerdict int

const (
	// driveOK is a 2xx: the leg served.
	driveOK driveVerdict = iota
	// driveRetry means "not ready yet" — keep polling until the budget runs out.
	driveRetry
	// driveTerminal means the response can never become a 2xx; fail now, with
	// the reason, while the engine's own logs still describe why.
	driveTerminal
)

// engineDeadMarkers are substrings of an engine error body that prove the
// MODEL RUNNER is gone rather than the request being bad. The parent
// `ollama serve` keeps answering /api/tags with 200 after its llama-server
// child dies, and the gateway forwards the child's error verbatim, so this
// body is the only signal that separates "engine broken" from "bad request".
var engineDeadMarkers = []string{
	"process has terminated",
	"model runner has unexpectedly stopped",
	"signal: segmentation fault",
	"signal: killed",
	"signal: aborted",
	"core dumped",
}

// engineDeadReason returns the marker proving the model runner died, or "".
func engineDeadReason(body []byte) string {
	s := string(body)
	for _, m := range engineDeadMarkers {
		if strings.Contains(s, m) {
			return m
		}
	}
	return ""
}

// retryableStatus reports whether a bare status code is plausibly transient.
// Used both for a direct response and for the local status recovered from the
// fallback header (where no local body survives).
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooEarly, // 425 — engine/model not ready yet
		http.StatusServiceUnavailable, // 503 — model_not_ready, runtime_unhealthy, …
		http.StatusTooManyRequests,    // 429
		http.StatusRequestTimeout:     // 408
		return true
	}
	return false
}

// localStatusFromFallback parses the intercept's `X-Waired-Fallback` value.
// A `local_status_<N>` reason yields the HTTP status the local gateway
// returned before the proxy discarded it; a `local_<name>` reason yields the
// staged X-Waired-Local-Error marker. Anything else (including the
// mirror-image `local; reason=anthropic_unreachable`) says nothing about a
// local failure.
func localStatusFromFallback(v string) (status int, named string, ok bool) {
	_, reason, found := strings.Cut(v, "reason=")
	if !found {
		return 0, "", false
	}
	reason = strings.TrimSpace(reason)
	rest, isLocal := strings.CutPrefix(reason, "local_")
	if !isLocal || rest == "" {
		return 0, "", false
	}
	if n, isStatus := strings.CutPrefix(rest, "status_"); isStatus {
		if code, err := strconv.Atoi(n); err == nil {
			return code, "", true
		}
	}
	return 0, rest, true
}

// classifyDrive decides whether a non-2xx drive response can still become a
// 2xx. CONSERVATIVE BY CONSTRUCTION: only signals that PROVE a dead or
// mis-wired engine are terminal, and everything unrecognised keeps retrying,
// so a genuinely cold model load still gets its full budget — the retry
// loop's original purpose.
//
// blackholed reports whether the run points api.anthropic.com at 0.0.0.0
// (the CI fail-open guard), which is what turns "cannot reach upstream" from
// a network blip into proof that the request already escaped local routing.
//
// expect is what the leg claims about its turn, and it changes which
// responses are terminal in BOTH directions. The blackhole's 502 branch
// below only ever saw the route=auto fail-open, because the route=anthropic
// leg degrades back to local (#665) and arrives as a 200 — so a leg that
// claims local must reject a 200 that carries that degrade, and a leg that
// claims upstream must accept the real API's 401 as its terminus rather
// than as "an auth/wiring regression" (waired-agent#1141).
func classifyDrive(status int, hdr http.Header, body []byte, blackholed bool, expect Outcome) (driveVerdict, string) {
	if status >= 200 && status < 300 {
		degraded := degradedFromAnthropic(hdr)
		// The zero Outcome is treated as the LOCAL claim on purpose: a leg
		// that forgets to declare one gets the strict check rather than no
		// check at all. Only an explicit upstream claim relaxes it.
		switch {
		case expect != outcomeUpstream && degraded:
			why := "the turn left local routing and was served here only because the upstream " +
				"could not be reached (" + headerFallback + ": local; reason=" +
				reasonAnthropicUnreachable + ") — this leg claims to be served on this device, " +
				"and a reachable upstream would have taken the turn off it"
			if blackholed {
				why += "; this lane blackholes api.anthropic.com, which is what made the escape look like success"
			}
			return driveTerminal, why
		case expect == outcomeUpstream && !degraded:
			return driveTerminal, "the turn was served locally, but this leg names a model the real " +
				"Anthropic API serves, which pins it upstream (waired-agent#1091) — a local 2xx here " +
				"means the route decision regressed"
		}
		return driveOK, ""
	}

	// The real API answering at all is what an upstream-claiming leg is
	// for. The legs send a deliberately-bogus key, so 401/403 IS the
	// upstream's answer; the route the daemon recorded is what the caller
	// then asserts on.
	if expect == outcomeUpstream && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return driveOK, ""
	}

	// The engine said its own runner is gone. True on any status, and the
	// most useful reason available, so it wins.
	if m := engineDeadReason(body); m != "" {
		return driveTerminal, fmt.Sprintf("the local model runner is dead (engine said %q); "+
			"no amount of retrying brings it back", m)
	}

	// Un-mask the Claude proxy's fail-open: the local error it discarded is
	// recoverable from the header it leaves on the response.
	if fb := hdr.Get(headerFallback); fb != "" {
		if local, named, ok := localStatusFromFallback(fb); ok {
			switch {
			case local != 0 && retryableStatus(local):
				return driveRetry, ""
			case local != 0:
				return driveTerminal, fmt.Sprintf(
					"the Claude proxy discarded a local HTTP %d and replayed upstream (%s)", local, fb)
			default:
				// A named local error (no_model, peer_ttfb_timeout, …).
				// Retry: the budget is the bound, and these can clear.
				return driveRetry, "local_" + named
			}
		}
	}

	if hdr.Get("Retry-After") != "" || retryableStatus(status) {
		return driveRetry, ""
	}

	switch status {
	case http.StatusBadGateway: // 502
		if blackholed && strings.Contains(string(body), "waired_upstream_unreachable") {
			return driveTerminal, "the request left local routing and CI blackholes api.anthropic.com — " +
				"the sentinel's core assertion (served locally, no fail-open) has already failed"
		}
		// e.g. engine_request_failed while the engine socket comes up.
		return driveRetry, ""
	case http.StatusInternalServerError: // 500
		return driveTerminal, "the gateway returned 500 (a code/config fault or a verbatim engine 5xx); neither self-heals"
	case http.StatusNotFound: // 404 model_not_found
		return driveTerminal, "the model the leg requested does not resolve (model_not_found) — a catalog/mapping regression"
	case http.StatusUnauthorized, http.StatusForbidden: // 401 / 403
		return driveTerminal, fmt.Sprintf("HTTP %d — an auth/wiring regression, and the tell that the request reached real Anthropic", status)
	case http.StatusBadRequest: // 400 capability_not_met
		return driveTerminal, "HTTP 400 — the request is rejected on its merits (capability_not_met), not on timing"
	case http.StatusUnprocessableEntity: // 422 hardware_insufficient
		return driveTerminal, "HTTP 422 — this host cannot serve the model (hardware_insufficient)"
	}
	return driveRetry, ""
}

// includedLeg applies the WAIRED_INTEGRATION_LEGS filter. A nil filter means
// "every leg".
func includedLeg(name string, only map[string]bool) bool {
	return only == nil || only[name]
}

// unknownLegs returns the filter names that match no leg, sorted.
//
// A filter is a request for specific legs, so a name that matches nothing
// is a mistyped request rather than a smaller run. Left alone it produces
// an empty selection, zero subtests, and exit 0 — a pass that measured
// nothing (waired-agent#1118). The same stance the agent-harness lane
// takes on a malformed WAIRED_AGENTGRADE_TRIALS.
//
// It takes the known names rather than calling legs() because legs() lives
// behind `//go:build integration` and this file deliberately does not, so
// the arithmetic here stays unit-testable. That also keeps the names in
// the test derived from one place instead of typed twice.
func unknownLegs(only map[string]bool, known []string) []string {
	if len(only) == 0 {
		return nil
	}
	have := make(map[string]bool, len(known))
	for _, n := range known {
		have[n] = true
	}
	var bad []string
	for n := range only {
		if !have[n] {
			bad = append(bad, n)
		}
	}
	sort.Strings(bad)
	return bad
}

// progressf streams one line to stderr NOW. t.Logf buffers until the subtest
// returns, which is useless for watching a hang: waired-agent#29 presented as
// three minutes of total silence per leg followed by a kill.
func progressf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "    [sentinel] "+format+"\n", args...)
}

// watchLeg reports every progressEvery that a phase is still running, so a
// stalled attempt is visible while it stalls. The returned func stops it.
func watchLeg(leg, phase string) (stop func()) {
	done := make(chan struct{})
	started := time.Now()
	go func() {
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				progressf("%s: %s still waiting after %s", leg, phase, time.Since(started).Round(time.Second))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
