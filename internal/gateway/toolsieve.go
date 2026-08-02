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
var toolSieveJSONKeys = []string{`"name"`, `"arguments"`, `"parameters"`}

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
