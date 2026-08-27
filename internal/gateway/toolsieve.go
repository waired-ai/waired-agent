package gateway

import (
	"bytes"
	"strings"
)

// The streaming half of #409.
//
// proxyAnthropicStream forwards each text delta to the client the moment
// it arrives, and Anthropic's SSE shape has no retraction event: once a
// text_delta is out, it is out. So by the time a leaked tool call is
// visible in full, it has already been shown to the user as prose and
// there is nothing left to convert. Buffering the whole turn instead
// would fix that and destroy time-to-first-byte, which the gateway
// spends real complexity defending (#757).
//
// toolTextSieve is the middle path: it releases text immediately until
// something in the stream could be the START of a leaked call, then
// holds only from that point. Ordinary prose therefore streams
// unchanged, and the hold lasts until the turn ends — at which point the
// held tail either parses as a call (and is replaced by a tool_use
// block) or is flushed verbatim.
//
// Claude Code always streams, so this — not the non-streaming path — is
// what makes the fix reach a real user.

// toolSieveSentinels are the markers whose appearance means "a tool call
// may be starting here".
//
// Deliberately NOT included: a bare ``` fence. Coding agents emit fenced
// code constantly, and holding every fence to its closer would stall
// progressive display across the common case to catch a rare one. The
// json-tagged fence is kept because it is the exact wrapper measured
// around qwen2.5-coder's leaked calls and is rare in ordinary prose.
var toolSieveSentinels = []string{
	"<function=", "<parameter=",
	"<tool_call>", "<function_call>", "<tools>",
	"<|tool_call|>", "[TOOL_CALLS]", "<|python_tag|>",
	"```json", "```JSON",
}

// toolSieveJSONKeys start the bare-JSON shape. An object opening with
// one of these keys is a candidate; `{` alone is not, because it appears
// in nearly every code block a coding agent writes.
//
// Both orderings are covered: every measured transcript put "name"
// first, but nothing in the format requires it.
var toolSieveJSONKeys = []string{`"name"`, `"function"`, `"arguments"`, `"parameters"`}

// toolSieveMaxHold bounds how much text is withheld before the sieve
// gives up and releases it. A tool call CAN legitimately be this large
// (a Write with a whole file body), so the cap is generous; it exists so
// a false-positive sentinel in a very long turn cannot pin the entire
// response in memory or blank the client for its whole duration. Past
// the cap the turn behaves exactly as it did before #409.
const toolSieveMaxHold = 1 << 20

// toolTextSieve withholds the suspicious tail of a streamed assistant
// message. The zero value is not usable; construct with newToolTextSieve.
type toolTextSieve struct {
	offered offeredTools
	pending []byte
	// holding is set once pending starts at a COMPLETE sentinel. Until
	// then pending holds at most a partial sentinel at the tail, which
	// the next delta may resolve into ordinary text.
	holding bool
}

func newToolTextSieve(tools []AnthropicTool) *toolTextSieve {
	return &toolTextSieve{offered: newOfferedTools(tools)}
}

// enabled reports whether recovery can fire at all. With no tools
// offered there is nothing a recovered name could be checked against, so
// the sieve degrades to a pass-through.
func (s *toolTextSieve) enabled() bool { return len(s.offered) > 0 }

// Push takes one content delta and returns the text that is safe to emit
// now. The remainder, if any, is held until Finish or Flush.
func (s *toolTextSieve) Push(delta string) string {
	if !s.enabled() {
		return delta
	}
	if s.holding {
		s.pending = append(s.pending, delta...)
		if len(s.pending) <= toolSieveMaxHold {
			return ""
		}
		// Over the cap: release everything and start scanning afresh.
		out := string(s.pending)
		s.pending, s.holding = nil, false
		return out
	}

	s.pending = append(s.pending, delta...)
	i, definite := suspicionStart(s.pending)
	if i >= len(s.pending) {
		out := string(s.pending)
		s.pending = s.pending[:0]
		return out
	}
	out := string(s.pending[:i])
	s.pending = append([]byte(nil), s.pending[i:]...)
	s.holding = definite
	return out
}

// Flush releases the held text unchanged and forgets it. Used when the
// turn already produced structured tool_calls, so recovery must not run
// — the engine's parser worked, and anything that looks like a second
// call in the text is the model's own prose.
func (s *toolTextSieve) Flush() string {
	out := string(s.pending)
	s.pending, s.holding = nil, false
	return out
}

// Finish ends the turn: it returns the text still owed to the client
// and, when the held tail parses as a call to a tool the request
// actually offered, that call with the fragment removed from the text.
func (s *toolTextSieve) Finish() (string, recoveredCall, bool) {
	if len(s.pending) == 0 {
		return "", recoveredCall{}, false
	}
	text := string(s.pending)
	s.pending, s.holding = nil, false
	c, ok := recoverToolCall(text, s.offered)
	if !ok {
		return text, recoveredCall{}, false
	}
	return stripFragment(text, c), c, true
}

// The whole-turn half of waired-agent#786.
//
// The sieve above holds text from the point something could START a
// leaked call. It cannot help with a turn that is leftover markup end to
// end — a CLOSING tag is not a sentinel (holding prose that happens to
// end in one would stall every such turn to catch a rare one), so text
// like `<response>` / `</function>` / `</tool_call>` streams out as
// ordinary prose and the usable-turn check saw text and was satisfied.
//
// markupWatch records what actually reached the client so the verdict at
// the end of the turn can ask a different question: was ANY of this
// prose? It is deliberately not a second recovery attempt — recovery
// already ran and found no call it could name.

// markupWatchCap bounds what markupWatch remembers. A turn that streamed
// more than this is not the failure mode: the measured cases are a
// handful of stray tags, and a long answer that ends with a stray tag is
// still an answer. Past the cap the watch reports "not markup only"
// without having to hold the turn in memory.
const markupWatchCap = 4 << 10

// markupWatch accumulates emitted assistant text, up to markupWatchCap.
type markupWatch struct {
	seen     []byte
	overflow bool
}

func newMarkupWatch() *markupWatch { return &markupWatch{} }

func (m *markupWatch) add(s string) {
	if m == nil || m.overflow {
		return
	}
	if len(m.seen)+len(s) > markupWatchCap {
		m.seen, m.overflow = nil, true
		return
	}
	m.seen = append(m.seen, s...)
}

// onlyEngineMarkup reports whether every byte the client received was
// tool-call markup and whitespace, with at least one such marker present.
// An empty turn is NOT markup-only: a turn with no text at all is the
// thinking-only fault (#442), which the caller already tells apart.
func (m *markupWatch) onlyEngineMarkup() bool {
	if m == nil || m.overflow {
		return false
	}
	return textIsOnlyEngineMarkup(string(m.seen))
}

// text returns the visible text seen so far, or "" once the watch has
// overflowed its cap (it stops accumulating, so what it holds would be
// a partial view presented as the whole).
func (m *markupWatch) text() string {
	if m == nil || m.overflow {
		return ""
	}
	return string(m.seen)
}

// engineMarkupTagNames are the tag names the leaked dialects use. Taken
// from the shapes toolRecovery* parses plus `response`, which is what a
// mesh-served qwen3.5-2b opened its markup-only turn with under the
// Claude Code harness (waired-agent#786). Matching is on the tag NAME,
// so both the opening form (with or without an `=value` attribute) and
// the closing form are covered by one entry.
var engineMarkupTagNames = map[string]bool{
	"tool_call":     true,
	"function":      true,
	"function_call": true,
	"parameter":     true,
	"parameters":    true,
	"tools":         true,
	"response":      true,
	"python_tag":    true,

	// Reasoning channels, which leak the same way and for the same
	// reason: the engine's parser did not split a channel the template
	// emitted, so it arrives as visible assistant text. Measured against
	// real Claude Code driving this stack — "the visible assistant text
	// carried the model's raw chain-of-thought and a bare </think>"
	// (internal/e2e/agentgrade/hold_test.go) — and counted since by
	// scripts/dev/agentgrade-contract.py, which reports it out of band
	// because nothing in the product looked for it.
	//
	// A turn that is ONLY a reasoning trace is not an answer, which is
	// what this predicate decides. A turn that reasons AND answers keeps
	// its answer: the test is subtractive, so prose survives whichever
	// markers came with it.
	"think":    true,
	"thinking": true,
	"channel":  true,
	"analysis": true,
}

// engineMarkupBareMarkers are the leaked markers that are not `<...>`
// tags. Backtick fences are stripped separately: a fence around markup
// is still markup, and a fence around anything else leaves that
// something behind for the emptiness test to find.
var engineMarkupBareMarkers = []string{"[TOOL_CALLS]", "<|start|>assistant"}

// textIsOnlyEngineMarkup reports whether s consists solely of markup the
// engine should have consumed — a leaked tool call or a leaked reasoning
// channel — and whitespace.
//
// The test is subtractive on purpose: remove what is recognisably
// markup, and require that NOTHING else is left. A model that answers
// with prose keeps its prose whichever tags it also emitted, so the only
// way to reach a false positive is a turn whose entire content is
// tool-call tags — which is the defect.
func textIsOnlyEngineMarkup(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	var rest strings.Builder
	sawMarker := false
	for i := 0; i < len(s); {
		if s[i] == '<' {
			if j := strings.IndexByte(s[i:], '>'); j > 0 {
				if isEngineMarkupTag(s[i+1 : i+j]) {
					sawMarker = true
					i += j + 1
					continue
				}
			}
		}
		rest.WriteByte(s[i])
		i++
	}
	out := rest.String()
	for _, m := range engineMarkupBareMarkers {
		if strings.Contains(out, m) {
			sawMarker = true
			out = strings.ReplaceAll(out, m, "")
		}
	}
	out = strings.ReplaceAll(out, "```json", "")
	out = strings.ReplaceAll(out, "```JSON", "")
	out = strings.ReplaceAll(out, "```", "")
	return sawMarker && strings.TrimSpace(out) == ""
}

// reasoningLeakMarkers are the channel markers a model's reasoning
// arrives under when the engine's parser did not split it off.
//
// Measured against real Claude Code driving this stack: "the visible
// assistant text carried the model's raw chain-of-thought and a bare
// </think>" (internal/e2e/agentgrade/hold_test.go). The same list is
// counted out of band by scripts/dev/agentgrade-contract.py; this is the
// product finally looking for what that script was finding.
var reasoningLeakMarkers = []string{
	"<think>", "</think>",
	"<thinking>", "</thinking>",
	"<|channel|>", "<|message|>", "<|start|>",
}

// textLeaksReasoning reports whether visible assistant text carries a
// reasoning channel marker.
//
// A record of today's behaviour, not a contract: it is reported, never
// acted on. A turn that leaked its trace AND answered still answered,
// and dropping it would cost the user a reply to fix a presentation
// defect. Widening this into a usability verdict needs a ratifying
// source first.
func textLeaksReasoning(s string) bool {
	for _, m := range reasoningLeakMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isEngineMarkupTag reports whether the inside of a `<...>` is one of the
// leaked tool-call tags. Tolerates the closing slash, the `<|name|>`
// pipe form, and an `=value` or space-separated attribute after the
// name, because the measured dialects use all of them.
func isEngineMarkupTag(inner string) bool {
	name := strings.TrimSpace(inner)
	name = strings.TrimPrefix(name, "/")
	name = strings.Trim(name, "|")
	name = strings.TrimPrefix(name, "/")
	if i := strings.IndexAny(name, " =\t"); i >= 0 {
		name = name[:i]
	}
	return engineMarkupTagNames[strings.ToLower(strings.TrimSpace(name))]
}

// suspicionStart returns the earliest offset in buf from which text must
// be withheld, and whether that offset is a complete sentinel (definite)
// rather than a partial one at the tail that the next delta may resolve.
// An offset of len(buf) means nothing is suspicious.
func suspicionStart(buf []byte) (int, bool) {
	best, definite := len(buf), false
	consider := func(i int, sure bool) {
		if i < best || (i == best && sure && !definite) {
			best, definite = i, sure
		}
	}
	for _, m := range toolSieveSentinels {
		if i := bytes.Index(buf, []byte(m)); i >= 0 {
			consider(i, true)
			continue
		}
		if i := partialSuffixStart(buf, m); i >= 0 {
			consider(i, false)
		}
	}
	if i, sure := jsonCallSentinelStart(buf); i >= 0 {
		consider(i, sure)
	}
	return best, definite
}

// partialSuffixStart returns the offset at which buf ends with a proper
// prefix of marker, or -1. This is what keeps a sentinel split across
// two deltas ("<func" + "tion=Bash>") from streaming out as text.
func partialSuffixStart(buf []byte, marker string) int {
	max := len(marker) - 1
	if len(buf) < max {
		max = len(buf)
	}
	for k := max; k > 0; k-- {
		if bytes.HasSuffix(buf, []byte(marker[:k])) {
			return len(buf) - k
		}
	}
	return -1
}

// jsonCallSentinelStart finds the earliest '{' that opens an object
// whose first key is one of toolSieveJSONKeys, or that could still
// become one because the delta ended mid-key.
func jsonCallSentinelStart(buf []byte) (int, bool) {
	for i := bytes.IndexByte(buf, '{'); i >= 0; {
		rest := strings.TrimLeft(string(buf[i+1:]), " \t\r\n")
		for _, key := range toolSieveJSONKeys {
			if strings.HasPrefix(rest, key) {
				return i, true
			}
			// The object is still arriving: everything seen so far is
			// consistent with this key, so it cannot be released yet.
			if len(rest) < len(key) && strings.HasPrefix(key, rest) {
				return i, false
			}
		}
		next := bytes.IndexByte(buf[i+1:], '{')
		if next < 0 {
			return -1, false
		}
		i += 1 + next
	}
	return -1, false
}
