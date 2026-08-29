package integration

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestLegDriveBudget pins the worked numbers for the deadlines the sentinel
// actually runs under. PRODUCT CONTRACT: the 10m row is Go's default
// -timeout, which is what the harness gets when a caller forgets the flag.
func TestLegDriveBudget(t *testing.T) {
	cases := []struct {
		name      string
		until     time.Duration
		remaining int
		want      time.Duration
	}{
		// (10m - 60s)/4 - 45s = 90s. Four legs then fit in exactly 9m,
		// leaving runReserve for the binary to report its own failure.
		{"go-default-timeout", 10 * time.Minute, 4, 90 * time.Second},
		// (15m - 60s)/4 - 45s = 165s, capped at maxLegDriveBudget.
		{"chosen-timeout-hits-cap", 15 * time.Minute, 4, maxLegDriveBudget},
		{"single-leg-hits-cap", 10 * time.Minute, 1, maxLegDriveBudget},
		// Too little time for even one round of overhead: one attempt each,
		// no retries. A 0 budget is a legal answer -- the caller still makes
		// one attempt, so a failure always carries a real status.
		{"squeezed-timeout", 3 * time.Minute, 4, 0},
		// -timeout 0 (no deadline): the cap is the only bound.
		{"no-deadline", 0, 4, maxLegDriveBudget},
		{"no-legs", 10 * time.Minute, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := legDriveBudget(c.until, c.remaining); got != c.want {
				t.Errorf("legDriveBudget(%s, %d) = %s, want %s", c.until, c.remaining, got, c.want)
			}
		})
	}
}

// TestLegDriveBudgetInvariant is the assertion that makes waired-agent#29
// unrepresentable. That bug was pure arithmetic: 4 legs x 3min of retries
// exceeded go test's 10min default, so the last leg was killed mid-request
// and its t.Fatalf -- the one line naming the real HTTP status -- never
// printed. Every occurrence cost 10 minutes and produced only a goroutine
// dump.
//
// PRODUCT CONTRACT: whenever a leg gets a non-zero drive budget, all the
// remaining legs plus their non-drive overhead must fit inside the test
// binary's own deadline with runReserve to spare. Change any constant in a
// way that breaks this and ci.yml's always-on unit lane fails in seconds
// rather than the sentinel failing silently 15 minutes in.
func TestLegDriveBudgetInvariant(t *testing.T) {
	for _, until := range []time.Duration{3 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 25 * time.Minute} {
		for remaining := 1; remaining <= 8; remaining++ {
			budget := legDriveBudget(until, remaining)
			if budget == 0 {
				// Nothing fits; one attempt per leg is all we can promise.
				continue
			}
			worst := time.Duration(remaining) * (budget + legOverhead)
			if worst > until-runReserve {
				t.Errorf("until=%s remaining=%d: budget %s -> worst case %s exceeds %s (until - runReserve)",
					until, remaining, budget, worst, until-runReserve)
			}
		}
	}
}

// TestBudgetConstantsAreCoherent pins the relationships between the timing
// constants. PRODUCT CONTRACT: each of these is a way the harness could go
// silent again, so they are asserted rather than left to a reader to re-derive.
func TestBudgetConstantsAreCoherent(t *testing.T) {
	if driveAttemptTimeout > maxLegDriveBudget {
		t.Errorf("driveAttemptTimeout (%s) exceeds maxLegDriveBudget (%s): one attempt could consume a leg's whole budget",
			driveAttemptTimeout, maxLegDriveBudget)
	}
	if legOverhead < sentinelTimeout {
		t.Errorf("legOverhead (%s) does not cover sentinelTimeout (%s): the served-locally poll would eat into the next leg's budget",
			legOverhead, sentinelTimeout)
	}
	if driveRetryDelay >= driveAttemptTimeout {
		t.Errorf("driveRetryDelay (%s) is not small relative to driveAttemptTimeout (%s)",
			driveRetryDelay, driveAttemptTimeout)
	}
	if progressEvery >= driveAttemptTimeout {
		t.Errorf("progressEvery (%s) >= driveAttemptTimeout (%s): a stalled attempt would time out without ever reporting",
			progressEvery, driveAttemptTimeout)
	}
}

// TestWatchLegStopIsIdempotent pins that the progress watcher shuts down
// cleanly and that a double stop (defer + explicit) does not panic.
func TestWatchLegStopIsIdempotent(t *testing.T) {
	stop := watchLeg("claude", "drive attempt 1")
	stop()
	stop()
}

// ciSegfaultBody is the verbatim body the OpenCode/OpenClaw legs received in
// the waired-agent#29 failures (runs 30273349362 / 30278247061 / 30280425468).
// The gateway forwards the engine's own error through unchanged, so this is
// the only place the real reason reaches the wire.
const ciSegfaultBody = `{"error":{"message":"llama-server process has terminated: signal: segmentation fault (core dumped)","type":"api_error","param":null,"code":null}}`

// ciFailOpenBody is the verbatim body the two Claude legs received in the same
// runs: the intercept discarded the local 500 and replayed upstream, which CI
// blackholes, so the engine's death read as "cannot reach the upstream API".
const ciFailOpenBody = `{"error":{"message":"waired proxy could not reach the upstream API: dial tcp 0.0.0.0:443: connect: connection refused","type":"waired_upstream_unreachable"},"type":"error"}`

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// TestClassifyDrive pins retryable-vs-terminal. PRODUCT CONTRACT: the
// classifier is deliberately conservative -- anything it does not recognise
// keeps retrying, because the loop's original purpose (letting a cold model
// load finish) must survive. Only signals that PROVE the request can never
// become a 2xx are terminal.
func TestClassifyDrive(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		hdr        http.Header
		body       string
		blackholed bool
		expect     Outcome // zero value = the local claim, as in classifyDrive
		want       driveVerdict
		wantReason string // substring
	}{
		{name: "ok", status: 200, want: driveOK},
		{name: "ok-201", status: 201, want: driveOK},

		// The waired-agent#29 signature, both halves.
		{
			name: "ci-segfault-500", status: 500, body: ciSegfaultBody,
			want: driveTerminal, wantReason: "process has terminated",
		},
		{
			name: "ci-fail-open-502-unmasked-by-header", status: 502,
			hdr:  hdr(headerFallback, "anthropic; reason=local_status_500"),
			body: ciFailOpenBody, blackholed: true,
			want: driveTerminal, wantReason: "local HTTP 500",
		},
		// The row that stops the classifier being too eager: the SAME
		// fail-open shape during a cold load must still be retried.
		{
			name: "fail-open-502-over-a-transient-local-503", status: 502,
			hdr:  hdr(headerFallback, "anthropic; reason=local_status_503"),
			body: ciFailOpenBody, blackholed: true,
			want: driveRetry,
		},
		{
			name: "fail-open-502-named-local-reason-is-retried", status: 502,
			hdr:  hdr(headerFallback, "anthropic; reason=local_peer_ttfb_timeout"),
			body: ciFailOpenBody, blackholed: true,
			want: driveRetry,
		},
		// Without the blackhole an unreachable upstream is a real network
		// blip, not proof the sentinel's assertion already failed.
		{
			name: "fail-open-502-not-blackholed", status: 502, body: ciFailOpenBody,
			blackholed: false, want: driveRetry,
		},
		{
			name: "fail-open-502-blackholed-no-header", status: 502, body: ciFailOpenBody,
			blackholed: true, want: driveTerminal, wantReason: "blackhole",
		},

		// Genuinely transient: the cold-load window the retry loop exists for.
		{name: "too-early", status: 425, want: driveRetry},
		{name: "model-not-ready", status: 503, body: `{"error":{"type":"model_not_ready"}}`, want: driveRetry},
		{name: "runtime-unhealthy-no-retry-after", status: 503, body: `{"error":{"type":"runtime_unhealthy"}}`, want: driveRetry},
		{name: "engine-request-failed", status: 502, body: `{"error":{"type":"engine_request_failed"}}`, want: driveRetry},
		{name: "retry-after-header", status: 418, hdr: hdr("Retry-After", "5"), want: driveRetry},
		{name: "too-many-requests", status: 429, want: driveRetry},

		// Wiring/config regressions the sentinel exists to catch: no amount
		// of waiting fixes them.
		{name: "model-not-found", status: 404, body: `{"error":{"type":"model_not_found"}}`, want: driveTerminal},
		{name: "unauthorized", status: 401, want: driveTerminal},
		{name: "forbidden", status: 403, want: driveTerminal},
		{name: "capability-not-met", status: 400, want: driveTerminal},
		{name: "hardware-insufficient", status: 422, want: driveTerminal},

		// Default-safe: unrecognised means keep trying.
		{name: "unknown-teapot", status: 418, body: "\x00\x01garbage", want: driveRetry},

		// --- the routing claim (waired-agent#1141) ---
		//
		// A 2xx carrying the anthropic-unreachable degrade is NOT the local
		// serving a local-claiming leg asserts: the turn was routed upstream
		// and only came back because this lane blackholes api.anthropic.com
		// (#665). Before this row the sentinel accepted it, which is how
		// #1091 inverted a leg's meaning and stayed green on every PR.
		{
			name: "local-claim-rejects-the-anthropic-degrade", status: 200,
			hdr:        hdr(headerFallback, "local; reason=anthropic_unreachable"),
			blackholed: true, want: driveTerminal, wantReason: "blackholes api.anthropic.com",
		},
		// Terminal WITHOUT the blackhole too: the header says the turn left
		// local routing, and why the upstream was missing does not change that.
		{
			name: "local-claim-rejects-the-degrade-unblackholed", status: 200,
			hdr:        hdr(headerFallback, "local; reason=anthropic_unreachable"),
			blackholed: false, want: driveTerminal, wantReason: "left local routing",
		},
		{name: "local-claim-plain-200-is-fine", status: 200, want: driveOK},
		// Spelled out, it must behave exactly like the zero value above.
		{
			name: "explicit-local-claim-rejects-the-degrade", status: 200, expect: outcomeLocal,
			hdr:        hdr(headerFallback, "local; reason=anthropic_unreachable"),
			blackholed: true, want: driveTerminal, wantReason: "left local routing",
		},

		// An upstream-claiming leg sends a deliberately-bogus key, so the
		// real API's 401 IS its terminus rather than "an auth regression".
		{name: "upstream-claim-401-is-the-terminus", status: 401, expect: outcomeUpstream, want: driveOK},
		{name: "upstream-claim-403-is-the-terminus", status: 403, expect: outcomeUpstream, want: driveOK},
		// On a blackholed lane the same leg gets the degrade instead, and
		// that header is what proves the route WAS anthropic.
		{
			name: "upstream-claim-accepts-the-degrade", status: 200, expect: outcomeUpstream,
			hdr:        hdr(headerFallback, "local; reason=anthropic_unreachable"),
			blackholed: true, want: driveOK,
		},
		// A plain local 2xx for an id the real API serves means the route
		// decision regressed (#1091).
		{
			name: "upstream-claim-rejects-a-plain-local-200", status: 200, expect: outcomeUpstream,
			want: driveTerminal, wantReason: "route decision regressed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := classifyDrive(c.status, c.hdr, []byte(c.body), c.blackholed, c.expect)
			if got != c.want {
				t.Errorf("classifyDrive(%d, blackholed=%v) = %v (%q), want %v",
					c.status, c.blackholed, got, reason, c.want)
			}
			if c.wantReason != "" && !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, c.wantReason)
			}
			if got == driveTerminal && reason == "" {
				t.Error("a terminal verdict must carry a reason")
			}
		})
	}
}

func TestLocalStatusFromFallback(t *testing.T) {
	cases := []struct {
		in         string
		wantStatus int
		wantNamed  string
		wantOK     bool
	}{
		{"anthropic; reason=local_status_500", 500, "", true},
		{"anthropic; reason=local_status_503", 503, "", true},
		{"anthropic; reason=local_no_model", 0, "no_model", true},
		{"anthropic; reason=local_peer_ttfb_timeout", 0, "peer_ttfb_timeout", true},
		// The mirror-image header (upstream unreachable -> served locally)
		// says nothing about a local failure.
		{"local; reason=anthropic_unreachable", 0, "", false},
		{"anthropic; reason=local_status_notanumber", 0, "status_notanumber", true},
		{"", 0, "", false},
		{"anthropic", 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			status, named, ok := localStatusFromFallback(c.in)
			if status != c.wantStatus || named != c.wantNamed || ok != c.wantOK {
				t.Errorf("localStatusFromFallback(%q) = (%d, %q, %v), want (%d, %q, %v)",
					c.in, status, named, ok, c.wantStatus, c.wantNamed, c.wantOK)
			}
		})
	}
}

// TestOutcomeVocabulary pins the strings the run summary is written in.
//
// FOUR places agree on them and only one is Go: the harness writes
// "<leg> <outcome>" per line, and scripts/dev/lib/installtest-integration.sh,
// scripts/dev/installtest-macos.sh and scripts/dev/installtest-windows.ps1
// each compare field 2 against these literals to decide whether the run
// settled. Rename one here and every wrapper silently reports a healthy run
// as "no leg reached an assertion" — the same shape as the duplicated
// assertion sentence waired-agent#1118 removed.
func TestOutcomeVocabulary(t *testing.T) {
	for _, c := range []struct {
		got  Outcome
		want string
	}{
		{outcomeRan, "ran"},
		{outcomeLocal, "local"},
		{outcomeUpstream, "upstream"},
	} {
		if string(c.got) != c.want {
			t.Errorf("outcome literal = %q, want %q — the three installtest wrappers match this "+
				"exact string when counting which legs settled", c.got, c.want)
		}
	}
}

// TestDegradedFromAnthropic separates the two directions the fallback header
// travels. They are mirror images and easy to confuse: an "anthropic;
// reason=local_*" value means the LOCAL leg failed and the turn was replayed
// upstream, while "local; reason=anthropic_unreachable" means the turn was
// routed UPSTREAM and served here because the upstream was missing. Only the
// second one says a local-claiming leg's turn escaped.
func TestDegradedFromAnthropic(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"local; reason=anthropic_unreachable", true},
		// The other direction: a local failure replayed upstream.
		{"anthropic; reason=local_status_500", false},
		{"anthropic; reason=local_no_model", false},
		// Near misses.
		{"local; reason=anthropic_unreachable_later", false},
		{"local", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := degradedFromAnthropic(hdr(headerFallback, c.in)); got != c.want {
				t.Errorf("degradedFromAnthropic(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
	if degradedFromAnthropic(http.Header{}) {
		t.Error("a response with no fallback header is not a degrade")
	}
}

// TestUnknownLegs: a filter that names nothing real is a mistyped request,
// not a smaller run. Before waired-agent#1118 it produced an empty
// selection, zero subtests and exit 0 — and the wrapper reported that as
// "every leg served locally".
func TestUnknownLegs(t *testing.T) {
	known := []string{"claude", "claude-anthropic-model-id", "opencode", "openclaw"}

	if bad := unknownLegs(nil, known); bad != nil {
		t.Errorf("no filter must name nothing unknown, got %v", bad)
	}
	if bad := unknownLegs(map[string]bool{"claude": true, "openclaw": true}, known); bad != nil {
		t.Errorf("a filter of real legs must name nothing unknown, got %v", bad)
	}
	// The name most likely to be mistyped is the longest one.
	got := unknownLegs(map[string]bool{"claude": true, "claude-anthropic-model": true}, known)
	if len(got) != 1 || got[0] != "claude-anthropic-model" {
		t.Errorf("unknownLegs = %v, want [claude-anthropic-model]", got)
	}
	// Sorted, so the message is stable across map iteration order.
	got = unknownLegs(map[string]bool{"zzz": true, "aaa": true}, known)
	if len(got) != 2 || got[0] != "aaa" || got[1] != "zzz" {
		t.Errorf("unknownLegs = %v, want [aaa zzz] in order", got)
	}
}

// TestLegNamesAreTheOnesTheDocPromises pins the example in Env.Only's doc
// comment against the real table. The comment says `"claude,opencode"`,
// and an operator who pastes it after a rename gets a silent empty
// selection — which is the failure above, arriving through prose.
//
// legs() is behind the integration tag, so this asserts the two names the
// doc uses are still spelled that way in legs.go's source rather than
// calling it.
func TestLegNamesAreTheOnesTheDocPromises(t *testing.T) {
	src, err := os.ReadFile("legs.go")
	if err != nil {
		t.Fatalf("read legs.go: %v", err)
	}
	for _, name := range []string{"claude", "opencode"} {
		if !regexp.MustCompile(`Name:\s+"` + name + `",`).Match(src) {
			t.Errorf("Env.Only's doc comment offers %q as an example and legs.go no longer "+
				"declares a leg by that name — the example now selects nothing", name)
		}
	}
}

// TestIncludedLeg covers the WAIRED_INTEGRATION_LEGS filter, which had no test.
func TestIncludedLeg(t *testing.T) {
	if !includedLeg("claude", nil) {
		t.Error("a nil filter must include every leg")
	}
	only := map[string]bool{"claude": true, "opencode": true}
	if !includedLeg("claude", only) {
		t.Error("claude must be included")
	}
	if includedLeg("openclaw", only) {
		t.Error("openclaw must be excluded")
	}
}
