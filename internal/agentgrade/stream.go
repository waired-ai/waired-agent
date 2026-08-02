package agentgrade

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// readAnthropicStream folds an Anthropic SSE turn back into the
// non-streaming response shape.
//
// The point of reassembling here is that Classify never learns which
// transport produced the turn. Coding agents always stream, so the probe
// has to be able to measure the streaming path — waired-ai/waired-agent#409
// recovers a leaked tool call there through a completely different
// implementation (toolTextSieve, which must decide what to withhold
// before it has seen the end of the turn) than on the whole-body path.
// But a classifier that forked per transport would start reporting "this
// model behaves differently over SSE" when what it actually saw was two
// of OUR code paths disagreeing. Folding the stream back into the same
// block list keeps the model as the only variable.
//
// Usage is not reconstructed faithfully: the gateway's message_start
// carries input_tokens 0 and the real count never reaches the stream.
// Nothing in grading reads usage, and inventing a number here would be
// worse than the zero.
func readAnthropicStream(r io.Reader) (gateway.AnthropicResponse, error) {
	sc := bufio.NewScanner(r)
	// A tool call's arguments arrive as one partial_json frame in this
	// gateway, and a 2048-token turn can carry a large one; the default
	// 64 KB line limit would silently split it.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		blocks   []*streamBlock
		open     = map[int]*streamBlock{}
		out      = gateway.AnthropicResponse{Type: "message", Role: "assistant"}
		sawStop  bool
		sawStart bool
	)

	for sc.Scan() {
		// An SSE frame is "event: <name>" then "data: <json>". The
		// payload carries its own "type", so the event line is redundant
		// and only the data line is read.
		payload, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			break
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return gateway.AnthropicResponse{},
				fmt.Errorf("agentgrade: decode SSE payload: %w (%s)", err, truncate(payload))
		}

		switch ev.Type {
		case "message_start":
			sawStart = true
			out.ID = ev.Message.ID
			out.Model = ev.Message.Model
			if ev.Message.Role != "" {
				out.Role = ev.Message.Role
			}
			out.Usage.InputTokens = ev.Message.Usage.InputTokens

		case "content_block_start":
			// Appended rather than keyed by index alone: index is a label
			// on the wire, not an identity, and the gateway reuses one for
			// the recovered call. Two blocks that share an index still
			// have to survive as two blocks.
			b := &streamBlock{
				typ:  ev.ContentBlock.Type,
				id:   ev.ContentBlock.ID,
				name: ev.ContentBlock.Name,
			}
			blocks = append(blocks, b)
			open[ev.Index] = b

		case "content_block_delta":
			b := open[ev.Index]
			if b == nil {
				// Dropping this silently is the one failure that would
				// corrupt a measurement rather than fail it: the missing
				// fragment is usually the leaked tool call itself, and the
				// verdict recorded would be "the model never called a tool".
				return gateway.AnthropicResponse{},
					fmt.Errorf("agentgrade: %s delta for index %d with no open content block",
						ev.Delta.Type, ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
			case "thinking_delta":
				b.thinking.WriteString(ev.Delta.Thinking)
			case "input_json_delta":
				b.input.WriteString(ev.Delta.PartialJSON)
			}

		case "content_block_stop":
			delete(open, ev.Index)

		case "message_delta":
			if ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
			}
			out.Usage.OutputTokens = ev.Usage.OutputTokens

		case "message_stop":
			sawStop = true
		}
	}
	if err := sc.Err(); err != nil {
		return gateway.AnthropicResponse{}, fmt.Errorf("agentgrade: read SSE: %w", err)
	}
	if !sawStart || !sawStop {
		// A stream cut short folds up into a perfectly well-formed short
		// answer, and "the model did not attempt a tool call" is what
		// would be filed. Refuse instead: an incomplete run is not a
		// measurement (same rule as GradeUnknown).
		return gateway.AnthropicResponse{},
			fmt.Errorf("agentgrade: incomplete SSE turn (message_start=%t, message_stop=%t)", sawStart, sawStop)
	}

	out.Content = make([]gateway.AnthropicContentBlock, 0, len(blocks))
	for _, b := range blocks {
		blk := gateway.AnthropicContentBlock{Type: b.typ, ID: b.id, Name: b.name}
		switch b.typ {
		case "thinking":
			blk.Thinking = b.thinking.String()
		case "tool_use":
			raw := strings.TrimSpace(b.input.String())
			if raw == "" {
				// content_block_start already declared an empty input
				// object; an absent delta means it stayed empty.
				raw = "{}"
			}
			blk.Input = json.RawMessage(raw)
		default:
			// text, and anything a future gateway adds: keep whatever
			// prose came through rather than dropping the block, so an
			// unrecognised type still reaches the classifier as content.
			blk.Text = b.text.String()
		}
		out.Content = append(out.Content, blk)
	}
	return out, nil
}

// streamBlock accumulates one content block across its deltas.
type streamBlock struct {
	typ      string
	id       string
	name     string
	text     strings.Builder
	thinking strings.Builder
	input    strings.Builder
}

// streamEvent is the union of the SSE payloads the gateway emits. One
// struct rather than a type switch: the field sets do not overlap, and
// decoding twice to find out which event this is would cost more than
// the unused fields do.
type streamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message struct {
		ID    string                 `json:"id"`
		Role  string                 `json:"role"`
		Model string                 `json:"model"`
		Usage gateway.AnthropicUsage `json:"usage"`
	} `json:"message"`

	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`

	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		// StopReason rides on message_delta, where the spec allows null;
		// decoding null into a string leaves it empty, which is the same
		// as absent for our purposes.
		StopReason string `json:"stop_reason"`
	} `json:"delta"`

	Usage gateway.AnthropicUsage `json:"usage"`
}
