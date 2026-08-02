package agentgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// DefaultTimeout bounds one case. Generous on purpose: the probe runs
// against a cold model on a host that may be spilling to system RAM,
// and a timeout that fires on a slow-but-working host would be recorded
// as VerdictError — which is the right classification, but a useless
// run. Speed is measured elsewhere (the boot benchmark); this is a
// correctness probe.
const DefaultTimeout = 5 * time.Minute

// DefaultTrials is how many times the case set runs before a verdict is
// issued.
//
// Not a robustness flourish — a correction. The first full sweep of the
// catalog graded qwen3.5 0.8b, 2b and 4b as failing; an immediate
// re-run of the same models against the same fixture graded all three
// as passing. Single-shot measurement reports a coin flip as a verdict
// for anything near the boundary, and a store full of coin flips is no
// better than the reputation-based judgement this package exists to
// replace.
//
// Three is chosen against cost, not derived: the probe makes two
// tool-requiring calls per trial, so three trials is six chances to
// misformat, and models that are actually stable (qwen2.5-coder in one
// direction, qwen3.6 in the other) agreed across every run.
const DefaultTrials = 3

// Probe drives the fixture at a gateway surface.
type Probe struct {
	// BaseURL is the Anthropic-shaped surface to drive. In production
	// that is the Claude intercept proxy the coding agent's
	// ANTHROPIC_BASE_URL points at, so the probe exercises the same
	// path a real session takes: intercept → gateway → engine.
	BaseURL string

	// APIKey is sent as x-api-key. The local gateway does not check it;
	// a deliberately-bogus value means a regression that fails open to
	// the real upstream gets rejected there rather than silently
	// producing a compliant-looking answer from a hosted model. Grading
	// a local model on a hosted model's reply would be the worst
	// possible failure of this package.
	APIKey string

	// Client is the HTTP client. nil uses one bounded by Timeout.
	Client *http.Client

	// Timeout bounds each case. Zero means DefaultTimeout.
	Timeout time.Duration

	// Trials is how many times the case set runs. Zero means
	// DefaultTrials. See DefaultTrials for why one is not enough.
	Trials int

	// Stream drives the SSE path instead of the whole-body one.
	//
	// It is a knob rather than the default because the two answer
	// different questions and both are worth asking. A coding agent
	// always streams, so this is the path a real session takes; the
	// whole-body path is the one #322's history was measured on, and is
	// still what a non-streaming client gets. Since #409 they no longer
	// share an implementation of tool-call recovery, so agreement
	// between them is a fact to be checked rather than assumed — and a
	// disagreement is a gateway defect, not a property of the model.
	//
	// The response is folded back into the non-streaming shape before
	// classification (readAnthropicStream), so Classify cannot tell
	// which one ran.
	Stream bool
}

const defaultProbeAPIKey = "waired-agentgrade-probe-not-a-real-key"

// Grade is the whole-model verdict aggregated over the cases.
type Grade string

const (
	// GradePass: every case met its expectation. Warnings do not
	// prevent a pass.
	GradePass Grade = "pass"
	// GradeFail: at least one case is a model-quality failure.
	GradeFail Grade = "fail"
	// GradeUnknown: at least one case could not be measured, so the
	// model has no verdict. Explicitly not "fail" — see
	// waired-ai/waired-agent#203: an upstream failure recorded as a
	// quality verdict de-rates a model that was never tested.
	GradeUnknown Grade = "unknown"
)

// Report is one model's probe run.
type Report struct {
	Model string `json:"model"`
	Grade Grade  `json:"grade"`

	// Trials is how many times the case set ran.
	Trials int `json:"trials"`

	// Results carries, per case, the WORST verdict seen across trials.
	// Worst rather than last or most common: a model that hands a coding
	// agent unusable output one run in three is unusable, because a real
	// session makes hundreds of tool calls where this makes two.
	Results []Result `json:"results"`

	// Flaky names the cases that did not return the same verdict in
	// every trial. Reported rather than smoothed away: an unstable model
	// is a different thing from a bad one, and hiding which is which is
	// how a coin flip gets recorded as a measurement.
	Flaky []string `json:"flaky,omitempty"`

	// FixtureRevision is the probe input this run was measured against.
	// Set by Run so every writer carries it without having to remember:
	// a report without it can be imported as current when it is not, and
	// two runs at different weights are indistinguishable once both say
	// "pass".
	FixtureRevision string `json:"fixture_revision"`

	// AgentRevision is the waired-agent commit the probe ran from, and
	// Transport is the HTTP shape it drove. Together they identify the
	// HARNESS generation, which FixtureRevision deliberately does not
	// cover: #409 changed the gateway's answer for the same model at the
	// same fixture revision, and without these two a reader cannot tell
	// a pre-fix verdict from a post-fix one. See provenance.go.
	AgentRevision string `json:"agent_revision,omitempty"`
	Transport     string `json:"transport"`

	Started  time.Time `json:"started"`
	Duration string    `json:"duration"`
	// Error carries why the run could not be graded, when Grade is
	// GradeUnknown.
	Error string `json:"error,omitempty"`
}

// Run drives every case against one model and grades the outcome.
//
// It does not stop at the first failure: a table showing which cases
// failed is what tells a maintainer whether the model cannot format
// calls at all or merely over-calls on small talk, and re-running to
// find that out costs another model load.
func (p Probe) Run(ctx context.Context, model string) (Report, error) {
	trials := p.Trials
	if trials <= 0 {
		trials = DefaultTrials
	}
	rep := Report{
		Model:         model,
		Started:       time.Now(),
		Trials:        trials,
		AgentRevision: AgentRevision(),
		Transport:     TransportUnary,
	}
	if p.Stream {
		rep.Transport = TransportStream
	}
	rev, err := FixtureRevision()
	if err != nil {
		return rep, err
	}
	rep.FixtureRevision = rev
	names, err := ToolNames()
	if err != nil {
		return rep, err
	}

	// worst[case] keeps the most severe verdict seen; seen[case] tracks
	// whether every trial agreed; ran/failed count the ratio.
	worst := make(map[string]Result, len(Cases))
	seen := make(map[string]map[Verdict]bool, len(Cases))
	ran := make(map[string]int, len(Cases))
	failed := make(map[string]int, len(Cases))

	for t := 0; t < trials; t++ {
		for _, c := range Cases {
			res := p.one(ctx, model, c, names)
			if seen[c.Name] == nil {
				seen[c.Name] = map[Verdict]bool{}
			}
			seen[c.Name][res.Verdict] = true
			ran[c.Name]++
			if res.Verdict.IsFailure() {
				failed[c.Name]++
			}
			if prev, ok := worst[c.Name]; !ok || severity(res.Verdict) > severity(prev.Verdict) {
				worst[c.Name] = res
			}
			// An engine that cannot answer ends the run: further trials
			// would measure the same outage, slowly.
			if res.Verdict == VerdictError {
				rep.Results = collect(worst, seen, ran, failed, &rep)
				rep.Duration = time.Since(rep.Started).Round(time.Second).String()
				rep.Grade = GradeUnknown
				rep.Error = fmt.Sprintf("case %s: %s", res.Case, res.Detail)
				return rep, nil
			}
		}
	}

	rep.Results = collect(worst, seen, ran, failed, &rep)
	rep.Duration = time.Since(rep.Started).Round(time.Second).String()

	rep.Grade = GradePass
	for _, r := range rep.Results {
		if r.Verdict.IsFailure() {
			rep.Grade = GradeFail
		}
	}
	return rep, nil
}

// collect flattens the per-case worst results into Cases order and
// records which cases disagreed across trials.
func collect(worst map[string]Result, seen map[string]map[Verdict]bool,
	ran, failed map[string]int, rep *Report) []Result {
	out := make([]Result, 0, len(Cases))
	rep.Flaky = nil
	for _, c := range Cases {
		r, ok := worst[c.Name]
		if !ok {
			continue
		}
		r.Trials = ran[c.Name]
		r.FailedTrials = failed[c.Name]
		if len(seen[c.Name]) > 1 {
			rep.Flaky = append(rep.Flaky, c.Name)
			r.Detail = fmt.Sprintf("%s (%d of %d trials failed — not reproducible)",
				r.Detail, r.FailedTrials, r.Trials)
		}
		out = append(out, r)
	}
	return out
}

// severity orders verdicts so the worst one across trials survives.
// Error outranks everything because it means the run is not a
// measurement at all.
func severity(v Verdict) int {
	switch v {
	case VerdictError:
		return 5
	case VerdictMalformedToolCall, VerdictUnknownTool:
		return 4
	case VerdictUnstructuredToolCall:
		return 3
	case VerdictNoToolCall:
		return 2
	case VerdictUnpromptedToolCall:
		return 1
	default:
		return 0
	}
}

func (p Probe) one(ctx context.Context, model string, c Case, names map[string]bool) Result {
	req, err := BuildRequest(model, c)
	if err != nil {
		return Result{Case: c.Name, Verdict: VerdictError, Detail: "build request: " + err.Error()}
	}
	// Set here rather than inside BuildRequest: the fixture defines the
	// request's CONTENT — the weight FixtureRevision hashes and the
	// canary compares against the real client — and transport is not
	// part of that. Building it in would also move RequestBytes.
	req.Stream = p.Stream
	resp, err := p.post(ctx, req)
	if err != nil {
		// An upstream failure is normally not a verdict. The exception
		// is the engine refusing to parse what the model emitted: that
		// is the model failing, and letting it fall through to
		// VerdictError would let unparseable output escape grading
		// entirely (see VerdictMalformedToolCall).
		var ue *upstreamError
		if errors.As(err, &ue) && IsEngineParseFailure(ue.Body) {
			return Result{
				Case:     c.Name,
				Verdict:  VerdictMalformedToolCall,
				Detail:   "the engine could not parse the tool call this model emitted",
				Evidence: truncate(ue.Body),
			}
		}
		return Result{Case: c.Name, Verdict: VerdictError, Detail: err.Error()}
	}
	return Classify(c, resp, names)
}

// upstreamError carries a non-2xx from the gateway with its body, so
// the caller can tell an engine that is down from an engine rejecting
// the model's output.
type upstreamError struct {
	Status   int
	Body     string
	Fallback string
}

func (e *upstreamError) Error() string {
	if e.Fallback != "" {
		// Without this marker, "the model answered badly" and "the
		// request left local routing entirely" look identical from
		// here, and that ambiguity cost a week once (waired-agent#29).
		return fmt.Sprintf("HTTP %d (X-Waired-Fallback: %s — the request did not stay local): %s",
			e.Status, e.Fallback, truncate(e.Body))
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body))
}

func (p Probe) post(ctx context.Context, req gateway.AnthropicRequest) (gateway.AnthropicResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return gateway.AnthropicResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	key := p.APIKey
	if key == "" {
		key = defaultProbeAPIKey
	}
	httpReq.Header.Set("x-api-key", key)

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout + 30*time.Second}
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("post %s: %w", url, err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return gateway.AnthropicResponse{}, &upstreamError{
			Status:   httpResp.StatusCode,
			Body:     string(raw),
			Fallback: httpResp.Header.Get("X-Waired-Fallback"),
		}
	}

	// Read fully either way, then parse. The whole turn is bounded by
	// MaxTokens and the probe does not measure TTFB, so streaming the
	// parse would buy nothing — and reading first keeps the non-2xx path
	// above byte-identical across transports, which is what lets an
	// engine that refuses to parse the model's own tool call
	// (VerdictMalformedToolCall) still be detected on the SSE path.
	if p.Stream {
		return readAnthropicStream(bytes.NewReader(raw))
	}

	var out gateway.AnthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(raw)))
	}
	return out, nil
}
