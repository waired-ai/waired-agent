package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// Token metering (waired#829, public share spec §12).
//
// Every gateway surface records how many tokens a request consumed, so
// local telemetry finally carries token counts. On the peer overlay
// (:9474) the same sample additionally feeds the Public Share usage
// batcher via Deps.OnUsage — but the capture below is surface-agnostic
// and runs regardless, which is what makes the local-telemetry half of
// §12 hold on its own.
//
// Nothing here ever touches message content: only the upstream's own
// usage counters and the model identifier are read (§15-10).

// UsageSample is one completed request's metering, handed to
// Deps.OnUsage at the terminal point of the handler.
//
// EngineModel is the engine-native identifier the request actually ran
// against (an ollama tag / vLLM repo id), not the catalog model id: the
// control plane resolves a quality tier from it via
// proto/catalog.BestTier, which matches Source.Tag / Source.RepoID.
type UsageSample struct {
	Kind         string
	ModelID      string
	EngineModel  string
	Class        string
	InputTokens  int64
	OutputTokens int64
	DurationMS   int64
	// Status is the HTTP status the client saw. Consumers that bill or
	// report usage must ignore failures; local telemetry keeps them.
	Status int
}

// usageSnifferLimit caps the non-streaming accumulator. A chat
// completion response that exceeds it is not metered rather than held
// in memory; the streaming path has no such cap because it parses
// incrementally.
const usageSnifferLimit = 1 << 20 // 1 MiB

// openAIUsageEnvelope is the shape both the streaming and
// non-streaming OpenAI responses carry usage in.
type openAIUsageEnvelope struct {
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// usageSniffer extracts token counts from an upstream response while
// the bytes are being forwarded verbatim.
//
// It is a passive tee, never a buffer-then-forward: proxyToEngine
// writes each chunk to the client first and then feeds the same bytes
// here, so neither latency nor byte fidelity changes. A sniffer that
// cannot make sense of the stream simply yields nothing.
//
// Two modes, chosen from the upstream Content-Type:
//
//   - SSE: parses `data: {...}` lines incrementally and keeps the last
//     usage object seen. Handles chunks split at arbitrary byte
//     boundaries, since a Write may end mid-line.
//   - JSON: accumulates up to usageSnifferLimit and decodes once at the
//     end.
//
// A compressed response (any non-identity Content-Encoding) disables
// the sniffer: the alternative is dropping Accept-Encoding on the
// engine-facing request, which would change behaviour on the three
// surfaces that do not need metering at all.
type usageSniffer struct {
	sse     bool
	off     bool
	buf     bytes.Buffer
	partial []byte

	in, out int64
	seen    bool
}

// newUsageSniffer returns a sniffer for the given upstream response
// headers, or a disabled one when the body cannot be read as-is.
func newUsageSniffer(contentType, contentEncoding string) *usageSniffer {
	s := &usageSniffer{}
	if enc := strings.TrimSpace(strings.ToLower(contentEncoding)); enc != "" && enc != "identity" {
		s.off = true
		return s
	}
	s.sse = strings.Contains(strings.ToLower(contentType), "text/event-stream")
	return s
}

// Feed consumes a chunk that has already been forwarded to the client.
func (s *usageSniffer) Feed(p []byte) {
	if s == nil || s.off || len(p) == 0 {
		return
	}
	if !s.sse {
		if s.buf.Len()+len(p) > usageSnifferLimit {
			s.off = true
			s.buf.Reset()
			return
		}
		s.buf.Write(p)
		return
	}
	s.feedSSE(p)
}

// feedSSE scans complete lines, carrying any trailing partial line into
// the next call.
func (s *usageSniffer) feedSSE(p []byte) {
	data := p
	if len(s.partial) > 0 {
		data = append(s.partial, p...)
		s.partial = nil
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		s.consumeSSELine(bytes.TrimRight(data[:i], "\r"))
		data = data[i+1:]
	}
	// A pathological upstream that never emits a newline must not grow
	// this without bound.
	if len(data) > usageSnifferLimit {
		s.off = true
		s.partial = nil
		return
	}
	s.partial = append([]byte(nil), data...)
}

func (s *usageSniffer) consumeSSELine(line []byte) {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	s.decode(payload)
}

// decode records the usage object when present. Later chunks win: the
// OpenAI streaming contract puts usage in a final chunk (or repeats it),
// and the last one is authoritative.
func (s *usageSniffer) decode(b []byte) {
	var env openAIUsageEnvelope
	if err := json.Unmarshal(b, &env); err != nil || env.Usage == nil {
		return
	}
	s.in, s.out, s.seen = env.Usage.PromptTokens, env.Usage.CompletionTokens, true
}

// Usage returns the observed counts. ok is false when the upstream
// reported none — a legitimate outcome (an engine that omits usage, a
// client disconnecting mid-stream, a compressed or oversized body), and
// the caller records zero tokens rather than guessing.
func (s *usageSniffer) Usage() (in, out int64, ok bool) {
	if s == nil || s.off {
		return 0, 0, false
	}
	if !s.sse && s.buf.Len() > 0 && !s.seen {
		s.decode(s.buf.Bytes())
	}
	return s.in, s.out, s.seen
}

// usageSink is the Deps.OnUsage signature. Declared here so the
// gateway's own files can refer to it without repeating the func type.
type usageSink func(ctx context.Context, s UsageSample)
