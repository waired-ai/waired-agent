package agentgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// The request-shape matrix: does THIS model, on THIS engine build,
// render the message shapes a coding agent sends?
//
// Posted straight at the engine's OpenAI-compatible endpoint, never
// through this project's gateway. Both gateway surfaces fold a
// non-leading instruction turn into the leading system message
// (waired-agent#1035, #1055), so a row taken through the gateway would
// record a fact about our normaliser that is identical for every model.
// Engine-direct, the row is a fact about (weights, chat template,
// engine version) and stays true when our normalisation changes again.
//
// Engine version is part of the finding, not context: ollama 0.32.13
// rejects a non-leading system turn on qwen3.8, 0.32.14 tolerates it,
// and 0.32.15 merges it into the leading turn (ollama#17754, #17757,
// #17855). The same model and the same shape give different answers on
// different builds, so a record without a version says nothing.

// ShapeOutcome is what happened to one shape.
type ShapeOutcome string

const (
	// ShapeAccepted: the engine answered 2xx with a chat completion.
	ShapeAccepted ShapeOutcome = "accepted"
	// ShapeRejected: the engine answered non-2xx. This is a finding
	// about the model's template, not a broken run.
	ShapeRejected ShapeOutcome = "rejected"
	// ShapeError: nothing was measured — a transport failure, or a 2xx
	// that was not a chat completion. A report carrying one of these is
	// refused at import; a run that could not obtain an answer is not a
	// record.
	ShapeError ShapeOutcome = "error"
)

// Shape rejection markers, classified from the engine's own body via
// the gateway's matchers so the probe and the product agree about what
// a rejection is.
const (
	ShapeMarkerRequestShape = "shape_rejected"
	ShapeMarkerParseFailure = "parse_failure"
	ShapeMarkerOther        = "other"
)

// ShapeResult is one row of the matrix.
type ShapeResult struct {
	Shape   string       `json:"shape"`
	Digest  string       `json:"digest"`
	Outcome ShapeOutcome `json:"outcome"`
	Status  int          `json:"status"`

	// Marker classifies a rejection. Empty when the row was accepted.
	// The engine's own error text is deliberately NOT stored: it is
	// upstream's unstable wording, and an engine can echo request
	// content into it.
	Marker string `json:"marker,omitempty"`

	// SentRoles and EngineSawRoles are the message-role sequence as
	// posted and as the engine received it. Engine-direct they are equal
	// by construction, and that equality is the claim that the row is
	// the model's answer rather than ours.
	SentRoles      []string `json:"sent_roles"`
	EngineSawRoles []string `json:"engine_saw_roles"`

	// Detail carries a transport failure's reason, for a human reading
	// a failed run. Only set when Outcome is ShapeError.
	Detail string `json:"detail,omitempty"`
}

// ShapeReport is one model's matrix.
type ShapeReport struct {
	Model         string        `json:"model"`
	Engine        string        `json:"engine"`
	EngineVersion string        `json:"engine_version"`
	Results       []ShapeResult `json:"results"`

	// Expected and Measured close the oldest hole in this repository's
	// harnesses: a probe that skipped itself and reported success having
	// measured nothing (#956). They are compared at import and by the
	// GPU lane.
	Expected int `json:"shapes_expected"`
	Measured int `json:"shapes_measured"`

	// ControlOK reports whether the negative control drew a rejection.
	//
	// The control posts a model name the engine does not have and
	// requires a non-2xx. Without it, "every shape accepted" — the
	// answer most models give — is indistinguishable from a harness
	// that never reached a validating engine. It is deliberately
	// engine-independent: a control built on a renderer that rejects a
	// shape would itself break when the engine pin moves, which is
	// exactly what happened between 0.32.13 and 0.32.15.
	ControlOK bool `json:"control_ok"`

	// ControlDetail says why the control did not hold, when it did not.
	ControlDetail string `json:"control_detail,omitempty"`

	AgentRevision string        `json:"agent_revision"`
	Started       time.Time     `json:"started"`
	Duration      time.Duration `json:"duration_ns"`
}

// Valid reports whether the report is a measurement at all, and says
// why when it is not. "Could not draw the control" is not green, for
// the same reason an errored row is not.
func (r ShapeReport) Valid() error {
	if r.EngineVersion == "" {
		return fmt.Errorf("no engine version recorded: the same shape answers differently on different builds")
	}
	if r.Expected == 0 {
		return fmt.Errorf("the shape table was empty; nothing was asked")
	}
	if r.Measured != r.Expected {
		return fmt.Errorf("measured %d of %d shapes: a partial run is not a record", r.Measured, r.Expected)
	}
	for _, res := range r.Results {
		if res.Outcome == ShapeError {
			return fmt.Errorf("shape %q could not be measured: %s", res.Shape, res.Detail)
		}
	}
	if !r.ControlOK {
		detail := r.ControlDetail
		if detail == "" {
			detail = "the control was never run"
		}
		return fmt.Errorf("the negative control did not hold (%s): a run that cannot detect a rejection cannot report an acceptance", detail)
	}
	return nil
}

// Rejected returns the rows the engine refused.
func (r ShapeReport) Rejected() []ShapeResult {
	var out []ShapeResult
	for _, res := range r.Results {
		if res.Outcome == ShapeRejected {
			out = append(out, res)
		}
	}
	return out
}

// DefaultShapeTimeout bounds one shape. Nothing is generated
// (max_tokens is 1), so the only slow part is a cold model load.
const DefaultShapeTimeout = 10 * time.Minute

// ShapeProbe drives the matrix against a live engine.
type ShapeProbe struct {
	// EngineURL is the engine's OpenAI-compatible base, e.g. the
	// runtime adapter's BaseURL(). NOT this project's gateway.
	EngineURL string

	// EngineName and EngineVersion are read from the runtime adapter by
	// the caller, never typed by an operator: the version is the
	// finding.
	EngineName    string
	EngineVersion string

	Client  *http.Client
	Timeout time.Duration
}

// controlModelSuffix makes a model name no engine has. It is appended
// to the real tag so a human reading a failed control sees which run it
// came from.
const controlModelSuffix = "-waired-shape-control-absent"

// Run posts every shape and returns the matrix.
func (p ShapeProbe) Run(ctx context.Context, model string) (ShapeReport, error) {
	shapes := gateway.EngineShapes()
	rep := ShapeReport{
		Model:         model,
		Engine:        p.EngineName,
		EngineVersion: p.EngineVersion,
		Expected:      len(shapes),
		AgentRevision: AgentRevision(),
		Started:       time.Now(),
	}
	if len(shapes) == 0 {
		return rep, fmt.Errorf("agentgrade: the shape table is empty")
	}

	for _, s := range shapes {
		body, err := s.OpenAIBody(model)
		if err != nil {
			rep.Results = append(rep.Results, ShapeResult{
				Shape: s.Name, Digest: s.Digest(), Outcome: ShapeError,
				SentRoles: s.EngineRoles(), Detail: "render: " + err.Error(),
			})
			rep.Measured++
			continue
		}
		res := p.one(ctx, s, body)
		rep.Results = append(rep.Results, res)
		rep.Measured++
	}

	rep.ControlOK, rep.ControlDetail = p.control(ctx, model)
	rep.Duration = time.Since(rep.Started)
	return rep, nil
}

func (p ShapeProbe) one(ctx context.Context, s gateway.EngineShape, body []byte) ShapeResult {
	res := ShapeResult{
		Shape:          s.Name,
		Digest:         s.Digest(),
		SentRoles:      s.EngineRoles(),
		EngineSawRoles: s.EngineRoles(), // engine-direct: nothing rewrites the body
	}
	status, raw, err := p.post(ctx, body)
	res.Status = status
	switch {
	case err != nil:
		res.Outcome = ShapeError
		res.Detail = err.Error()
	case status >= 200 && status < 300:
		if !isChatCompletion(raw) {
			// A 2xx that is not a completion is not an acceptance. It
			// is a proxy, a stub, or an engine that answered something
			// else — and recording it as "accepted" is how a harness
			// reports success having measured nothing.
			res.Outcome = ShapeError
			res.Detail = "2xx body is not a chat completion"
			break
		}
		res.Outcome = ShapeAccepted
	default:
		res.Outcome = ShapeRejected
		res.Marker = classifyShapeRejection(string(raw))
	}
	return res
}

// control posts a model the engine does not have and requires a
// rejection. See ShapeReport.ControlOK.
func (p ShapeProbe) control(ctx context.Context, model string) (bool, string) {
	shapes := gateway.EngineShapes()
	body, err := shapes[0].OpenAIBody(model + controlModelSuffix)
	if err != nil {
		return false, "render: " + err.Error()
	}
	status, _, err := p.post(ctx, body)
	if err != nil {
		return false, err.Error()
	}
	if status >= 200 && status < 300 {
		return false, fmt.Sprintf("a request naming an absent model answered %d", status)
	}
	return true, ""
}

func (p ShapeProbe) post(ctx context.Context, body []byte) (int, []byte, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultShapeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimRight(p.EngineURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout + 30*time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// classifyShapeRejection names the kind of rejection using the
// gateway's own matchers, so the probe and the serving path cannot
// disagree about what a shape rejection is.
func classifyShapeRejection(body string) string {
	switch {
	case gateway.IsEngineRequestShapeRejection(body):
		return ShapeMarkerRequestShape
	case gateway.IsEngineParseFailure(body):
		return ShapeMarkerParseFailure
	default:
		return ShapeMarkerOther
	}
}

// isChatCompletion reports whether a 2xx body is the thing we asked
// for. Deliberately shallow: one choice with a message or a delta is
// enough to prove an engine answered.
func isChatCompletion(raw []byte) bool {
	var probe struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.Choices) > 0
}
