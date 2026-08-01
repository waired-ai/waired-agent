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
	Model   string   `json:"model"`
	Grade   Grade    `json:"grade"`
	Results []Result `json:"results"`

	// FixtureRevision is the probe input this run was measured against.
	// Set by Run so every writer carries it without having to remember:
	// a report without it can be imported as current when it is not, and
	// two runs at different weights are indistinguishable once both say
	// "pass".
	FixtureRevision string `json:"fixture_revision"`

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
	rep := Report{Model: model, Started: time.Now()}
	rev, err := FixtureRevision()
	if err != nil {
		return rep, err
	}
	rep.FixtureRevision = rev
	names, err := ToolNames()
	if err != nil {
		return rep, err
	}

	for _, c := range Cases {
		res := p.one(ctx, model, c, names)
		rep.Results = append(rep.Results, res)
	}
	rep.Duration = time.Since(rep.Started).Round(time.Second).String()

	rep.Grade = GradePass
	for _, r := range rep.Results {
		switch {
		case r.Verdict == VerdictError:
			rep.Grade = GradeUnknown
			if rep.Error == "" {
				rep.Error = fmt.Sprintf("case %s: %s", r.Case, r.Detail)
			}
			return rep, nil
		case r.Verdict.IsFailure():
			rep.Grade = GradeFail
		}
	}
	return rep, nil
}

func (p Probe) one(ctx context.Context, model string, c Case, names map[string]bool) Result {
	req, err := BuildRequest(model, c)
	if err != nil {
		return Result{Case: c.Name, Verdict: VerdictError, Detail: "build request: " + err.Error()}
	}
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

	var out gateway.AnthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(raw)))
	}
	return out, nil
}
