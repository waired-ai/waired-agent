package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AnthropicRequest mirrors POST /v1/messages. Only the Phase A subset
// is decoded into named fields; everything else round-trips through
// the raw map so we don't drop fields a future version of the spec
// might add.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []AnthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

// AnthropicMessage's Content can be string OR []AnthropicContentBlock;
// keep raw and let the conversion code pick.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicResponse is the non-streaming response shape.
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []AnthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason,omitempty"`
	Usage      AnthropicUsage          `json:"usage"`

	// ToolRecovery names the dialect a tool call was recovered from when
	// the engine left it in the assistant text (#409), or "" for the
	// ordinary path. json:"-" because it is gateway bookkeeping for
	// telemetry, not part of the Anthropic response the client sees —
	// the recovered call is already a normal tool_use block in Content.
	ToolRecovery string `json:"-"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// OpenAIRequest is the subset of /v1/chat/completions we synthesise
// from an AnthropicRequest. Only fields Ollama honours are populated.
type OpenAIRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Messages      []OpenAIMessage      `json:"messages"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
	Tools         []OpenAITool         `json:"tools,omitempty"`
	ToolChoice    json.RawMessage      `json:"tool_choice,omitempty"`
	Stop          []string             `json:"stop,omitempty"`
}

// OpenAIStreamOptions opts a streaming request in to a trailing usage
// chunk. OpenAI-compatible engines (Ollama included) only emit token
// usage on the stream when include_usage is set; without it the final
// message_delta reports output_tokens: 0.
type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Reasoning carries a thinking model's chain-of-thought on the
	// response decode path. Ollama's OpenAI-compat surface uses
	// `reasoning`; vLLM / DeepSeek / some llama.cpp builds use
	// `reasoning_content`. Both are omitempty so they never appear on
	// the request we build. Read them via reasoningText().
	Reasoning        string           `json:"reasoning,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
}

// reasoningText returns the model's reasoning trace, preferring the
// `reasoning` field and falling back to `reasoning_content` for engines
// that use the alternate key.
func reasoningText(m OpenAIMessage) string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.ReasoningContent
}

type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function OpenAIToolCallFunction `json:"function"`
}

type OpenAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// OpenAIResponse is the subset we decode from /v1/chat/completions.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrUnsupportedFeature is returned by AnthropicToOpenAI when the
// request asks for an Anthropic feature that Phase A intentionally
// declines (vision, extended thinking, cache_control, system as
// array, …). The handler maps it to a 400 with a documented code.
type ErrUnsupportedFeature struct{ Feature, Detail string }

func (e *ErrUnsupportedFeature) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("anthropic feature %q not supported in Phase A: %s", e.Feature, e.Detail)
	}
	return fmt.Sprintf("anthropic feature %q not supported in Phase A", e.Feature)
}

// AnthropicToOpenAI translates the request body. The original
// model field is preserved (the gateway will swap it for the
// engine-specific identifier later, after the router has run).
func AnthropicToOpenAI(req AnthropicRequest) (OpenAIRequest, error) {
	if req.MaxTokens <= 0 {
		// Anthropic spec requires max_tokens.
		return OpenAIRequest{}, errors.New("anthropic: max_tokens is required")
	}

	out := OpenAIRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Stop:        req.StopSequences,
		ToolChoice:  req.ToolChoice,
	}
	// Opt in to the trailing usage chunk so streamed responses can
	// report real output_tokens (see OpenAIStreamOptions).
	if req.Stream {
		out.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	// system may arrive as a plain string OR as an array of text blocks
	// (Claude Code always sends the array form, attaching cache_control to
	// the blocks for prompt caching). Flatten both into a single system
	// message; cache_control is dropped (the local engine doesn't cache).
	sysStr, err := anthropicSystemToString(req.System)
	if err != nil {
		return OpenAIRequest{}, err
	}
	if sysStr != "" {
		out.Messages = append(out.Messages, OpenAIMessage{Role: "system", Content: sysStr})
	}

	// messages
	for _, m := range req.Messages {
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return OpenAIRequest{}, err
		}
		out.Messages = append(out.Messages, converted...)
	}

	// tools
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	// top_k is not supported by OpenAI Chat Completions; we silently
	// drop it. Caller can choose to surface a warning via header.
	_ = req.TopK

	return out, nil
}

// anthropicSystemToString collapses the Anthropic `system` field into a
// single string. It accepts the two shapes Anthropic (and Claude Code)
// send: a plain JSON string, or an array of content blocks. Text blocks
// are concatenated (newline-joined); cache_control and other block
// metadata have no field in AnthropicContentBlock, so they're dropped on
// unmarshal. Non-text system blocks (none are emitted in practice) are
// skipped. A value that is neither a string nor a block array is a
// malformed request and surfaces as a 400.
func anthropicSystemToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("anthropic: system must be a string or array of blocks: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// convertAnthropicMessage translates a single Anthropic message,
// possibly fanning it out into multiple OpenAI messages (tool_result
// blocks become separate {role:"tool"} messages, per OpenAI's tool
// calling contract).
func convertAnthropicMessage(m AnthropicMessage) ([]OpenAIMessage, error) {
	// Try string-content first.
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		return []OpenAIMessage{{Role: m.Role, Content: asString}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("anthropic: content must be string or array of blocks: %w", err)
	}

	var out []OpenAIMessage
	var textParts []string
	var toolCalls []OpenAIToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: OpenAIToolCallFunction{Name: b.Name, Arguments: args},
			})
		case "tool_result":
			// tool_result becomes a separate role:"tool" message.
			content, err := stringifyToolResultContent(b.Content)
			if err != nil {
				return nil, err
			}
			// Flush accumulated text/tool_calls first so message
			// order matches Anthropic's order.
			if len(textParts) > 0 || len(toolCalls) > 0 {
				out = append(out, OpenAIMessage{
					Role:      m.Role,
					Content:   strings.Join(textParts, ""),
					ToolCalls: toolCalls,
				})
				textParts = nil
				toolCalls = nil
			}
			out = append(out, OpenAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    content,
			})
		case "image":
			return nil, &ErrUnsupportedFeature{Feature: "image", Detail: "vision content blocks land in Phase B"}
		case "thinking", "redacted_thinking":
			// Assistant reasoning blocks from a prior extended-thinking
			// turn (e.g. served by real Anthropic before the request was
			// routed to local inference). They have no OpenAI Chat
			// representation, so drop them rather than 400 the whole turn
			// — a model switch mid-conversation must not hard-fail.
			continue
		default:
			return nil, &ErrUnsupportedFeature{Feature: b.Type, Detail: "unknown content block"}
		}
	}

	if len(textParts) > 0 || len(toolCalls) > 0 {
		out = append(out, OpenAIMessage{
			Role:      m.Role,
			Content:   strings.Join(textParts, ""),
			ToolCalls: toolCalls,
		})
	}
	return out, nil
}

// stringifyToolResultContent collapses Anthropic tool_result content
// (string OR array of {type:"text",text}) into the plain string
// OpenAI's role:"tool" message expects.
func stringifyToolResultContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("anthropic: tool_result content must be string or text-block array: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", &ErrUnsupportedFeature{Feature: "tool_result_block_" + b.Type, Detail: "only text blocks in tool_result for Phase A"}
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, ""), nil
}

// OpenAIToAnthropic translates the non-streaming response back. The
// originalModel param is the user's requested model alias; we put
// that in the response.model field so the caller doesn't see the
// engine-specific identifier (Anthropic spec doesn't define what
// `model` should look like, but client SDKs cache by it).
//
// offered is the tool set the request carried. It is only consulted when
// the engine returned NO structured tool_calls, to recover a call the
// engine's own parser left in the assistant text (#409); pass nil to
// disable that entirely. A recovered call is reported via the returned
// response's ToolRecovery field.
func OpenAIToAnthropic(resp OpenAIResponse, originalModel string, offered []AnthropicTool) AnthropicResponse {
	out := AnthropicResponse{
		ID:    "msg_" + resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: originalModel,
		// Initialise content as an empty (but non-nil) slice so it
		// marshals as `[]` not `null` — Anthropic SDK clients
		// expect an array even when the model produced nothing
		// visible (e.g. when reasoning consumed the whole budget).
		Content: []AnthropicContentBlock{},
		Usage: AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) == 0 {
		return out
	}
	choice := resp.Choices[0]
	// Block order follows Anthropic convention: thinking → text →
	// tool_use. Thinking models (e.g. qwen3) return their reasoning in
	// message.reasoning; surface it as a thinking block so Claude Code
	// can display it instead of the model appearing to emit no thinking.
	if r := reasoningText(choice.Message); r != "" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type:     "thinking",
			Thinking: r,
		})
	}
	// #409: when the engine produced no structured tool_calls, the text
	// may be a tool call its parser failed to consume. Recover it before
	// the text block is built, so the fragment does not also ship as
	// prose. Guarded on len(ToolCalls)==0: an engine that parsed the
	// call correctly is never second-guessed.
	text := choice.Message.Content
	var recovered recoveredCall
	if len(choice.Message.ToolCalls) == 0 && text != "" {
		if c, ok := recoverToolCall(text, newOfferedTools(offered)); ok {
			recovered, out.ToolRecovery = c, c.Shape
			text = stripFragment(text, c)
		}
	}
	if text != "" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type: "text",
			Text: text,
		})
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	if out.ToolRecovery != "" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    recoveredToolUseID(resp.ID),
			Name:  recovered.Name,
			Input: recovered.Input,
		})
	}
	// Safety net: if the model produced no visible block at all and the
	// stop reason is a max_tokens truncation (e.g. reasoning ate the
	// whole budget on a build that emits no reasoning field), a bare
	// content:[] turn stalls the agentic loop ("No response requested.").
	// Emit one visible note so the client always gets an actionable turn.
	if len(out.Content) == 0 && choice.FinishReason == "length" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type: "text",
			Text: truncationNote,
		})
	}
	out.StopReason = mapFinishReason(choice.FinishReason)
	if out.ToolRecovery != "" {
		// The engine saw no tool call, so it reported finish_reason
		// "stop", which maps to end_turn. Left alone, the client would
		// treat a turn that IS a tool call as a finished answer and
		// never run the tool.
		out.StopReason = "tool_use"
	}
	return out
}

// recoveredToolUseID mints the id for a synthesised tool_use block.
// Anthropic ids only have to be unique within the turn (the client pairs
// tool_result to them there), and at most one call is recovered per
// response, so deriving it from the engine's completion id is enough —
// and makes the block's origin obvious in a transcript.
func recoveredToolUseID(respID string) string {
	if respID == "" {
		return "toolu_waired_recovered"
	}
	return "toolu_waired_recovered_" + respID
}

// truncationNote is the visible text emitted when a response truncates
// at max_tokens without producing any content, thinking, or tool call.
const truncationNote = "[waired: the model reached max_tokens before producing any output. Increase max_tokens to get a response.]"

// engineParseFailureMarkers are substrings that identify an upstream
// error as "the engine could not parse the tool call the MODEL emitted",
// as opposed to any other 5xx.
//
// Deliberately narrow. Every entry names a parse of generated content;
// none of them can be produced by an engine that is merely down,
// loading, out of memory, or missing the model. Widening it would make
// the gateway retry an outage, and would make agentgrade record an
// infrastructure failure as a model's verdict — the split this list
// exists to make, collapsing in the wrong direction.
//
// Add to this list only from an observed run, and say which model
// produced it.
var engineParseFailureMarkers = []string{
	"XML syntax error",        // ollama, qwen3.5:4b-q4_K_M — measured 2026-08-01
	"error parsing tool call", // ollama tool-call parser
	"invalid tool call",
	"failed to parse tool",
	"unexpected end of JSON input", // truncated/instructured call in a tool arg
}

// IsEngineParseFailure reports whether an upstream error body shows the
// engine rejecting the model's own tool-call output — a bad draw from
// the model, not a sick engine, and therefore worth another attempt.
func IsEngineParseFailure(body string) bool {
	for _, m := range engineParseFailureMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// maxStreamRetries bounds how many times a truncated stream is re-drawn
// before the turn is given up on (#442).
//
// Two, so three attempts in all. Each attempt is an independent draw, so
// the ~50% per-turn failure measured on qwen3.5:9b becomes ~12%: the
// step from two attempts to three is still worth a few seconds of
// prefill, and the one after it is not.
const maxStreamRetries = 2

// streamFailureNote is what a person reads when the engine gave up
// part-way through and every retry did too.
//
// It names the model twice over — "this model" and then the id — because
// either alone can miss: a reader who never chose the model by name will
// not connect an id to it, and a reader running several will not know
// which one this was. It says how many attempts were made, so that the
// obvious next move ("just send it again") is not the one they make. And
// it names the fix rather than the fault, because the fault is upstream
// and there is nothing in it for them to act on.
//
// No waired vocabulary: not "engine", not "upstream", not "the parser".
// Someone reading their agent transcript did not opt into any of it.
func streamFailureNote(model string, attempts int) string {
	subject := "this model"
	if model != "" {
		subject = fmt.Sprintf("this model (%s)", model)
	}
	tries := fmt.Sprintf("%d attempts", attempts)
	if attempts == 1 {
		tries = "1 attempt"
	}
	return fmt.Sprintf("[waired: %s could not deliver a usable reply after %s. "+
		"Some models write their tool calls in a form that cannot be read back. "+
		"Switching to a different model usually fixes it.]", subject, tries)
}

// recordedModel is the catalog id the request resolved to, for text a
// person reads. Deliberately not the model the CLIENT asked for: Claude
// Code sends a claude-* alias, which names nothing the user chose.
func recordedModel(rr *requestRec) string {
	if rr == nil {
		return ""
	}
	return rr.ev.Model
}

func mapFinishReason(openai string) string {
	switch openai {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	}
	return openai
}

// CountTokensApprox returns a quick token-count estimate for an
// Anthropic /count_tokens request. Phase A uses a rough
// 1-token-per-4-bytes heuristic plus a per-message overhead. The
// response includes a Warning header (set by the handler) so clients
// know it's not exact; Phase B will use the model's real tokenizer.
//
// It counts the CONVERTED request — the bytes this gateway would send
// to the engine — rather than walking the Anthropic shape itself. The
// walk it used to do missed two of the three things a coding-agent
// request is mostly made of: the tool schemas (never counted on either
// side) and the tool_result payloads that carry file reads and command
// output (counted by the server, not by the requester). Both omissions
// pushed the estimate down, which is the direction that lets an
// over-window prompt through to be truncated at the head.
//
// Counting the converted form also removes the drift risk the two
// hand-written walks carried: the requester and the peer now count the
// same bytes, so neither can refuse a prompt the other passed
// (waired-agent#436).
func CountTokensApprox(req AnthropicRequest) int {
	if oai, err := AnthropicToOpenAI(req); err == nil {
		if encoded, err := json.Marshal(oai); err == nil {
			return CountOpenAIPromptTokensApprox(encoded)
		}
	}
	// A request this gateway cannot convert (an image block, an unknown
	// block type) never reaches the over-window guard — the handler
	// rejects it at conversion. Only /count_tokens gets here, so answer
	// from the request's own bytes rather than keeping a second walk
	// alive that nothing else exercises.
	if encoded, err := json.Marshal(req); err == nil {
		return approxTokenCount(string(encoded))
	}
	return 0
}

func approxTokenCount(s string) int {
	// 1 token ≈ 4 bytes for English; coarse but good enough for the
	// "give me a rough budget" use case count_tokens serves.
	if s == "" {
		return 0
	}
	n := (len(s) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// CountOpenAIPromptTokensApprox estimates the prompt size of a raw
// chat-completions body: 4 tokens of overhead per message, plus
// approxTokenCount of the content, the tool schemas and the tool-call
// arguments.
//
// This is the ONE counter. Both over-window guards read it — the
// requesting side via CountTokensApprox on the request it is about to
// forward, the serving side on the body it received — so the two cannot
// disagree about the same conversation (waired-agent#436).
//
// The three payload kinds are counted because they are what a
// coding-agent request is mostly made of, and every one of them was
// missing from one side or the other: tool schemas from both,
// tool-call arguments from this one, tool_result payloads from the
// Anthropic walk this replaced.
//
// Content is taken as json.RawMessage because a chat-completions
// message may carry either a string or an array of parts, and an
// unparseable body counts as 0 — the guard above it fails open, the
// same philosophy as every other unknown-sizing input here.
func CountOpenAIPromptTokensApprox(body []byte) int {
	var req struct {
		Messages []struct {
			Content   json.RawMessage  `json:"content"`
			ToolCalls []OpenAIToolCall `json:"tool_calls"`
		} `json:"messages"`
		Tools []OpenAITool `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0
	}
	const overheadPerMessage = 4
	total := 0
	for _, t := range req.Tools {
		total += approxTokenCount(t.Function.Name)
		total += approxTokenCount(t.Function.Description)
		total += approxTokenCount(string(t.Function.Parameters))
	}
	for _, m := range req.Messages {
		total += overheadPerMessage
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			total += approxTokenCount(s)
		} else {
			total += approxTokenCount(string(m.Content))
		}
		for _, tc := range m.ToolCalls {
			total += approxTokenCount(tc.Function.Name)
			total += approxTokenCount(tc.Function.Arguments)
		}
	}
	return total
}
