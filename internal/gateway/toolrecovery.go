package gateway

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Recovering a tool call the engine left in the assistant's text (#409).
//
// Engines parse the model's tool-call syntax themselves, and they get it
// wrong often enough that the failure dominates measured tool-call
// success. Measured against ollama 0.31.1 with a 27-tool coding-agent
// request: qwen2.5-coder (every bundled size) returned HTTP 200 with the
// call sitting in `content` as a fenced JSON object, 24/24 trials;
// qwen3-coder:30b-a3b leaked the Qwen3-Coder XML dialect 8/24;
// granite4:350m leaked its own [TOOL_CALLS] delimiter 5/12. In each case
// the model chose the RIGHT tool with the RIGHT arguments and only the
// serialisation was lost, so the gateway can put it back — one fix for
// every engine and all three operating systems, rather than waiting on
// upstream parsers.
//
// This file is the parsing half. The guards live at the call sites:
// recovery runs only when the request offered tools and the response
// carried no structured tool_calls, and a parsed name that was not
// offered is never converted (that is the #322 rc7 hallucination, and
// laundering it into a real call would be worse than the defect).
//
// Out of scope: responses where the engine returned 5xx because its own
// parser raised. There is no text to recover from, so that class needs
// either bypassing engine-side tool parsing entirely or an upstream fix.

// Recovery shape tags. Also the telemetry values, so a fired recovery is
// attributable to a dialect rather than only countable.
const (
	toolRecoveryXML       = "xml_function"
	toolRecoveryJSON      = "json_object"
	toolRecoveryDelimited = "delimited"
)

// recoveredCall is a tool call reconstructed from assistant text.
// Start/End bound the fragment within the scanned text so the caller can
// remove it and keep whatever prose surrounded it.
type recoveredCall struct {
	Name       string
	Input      json.RawMessage
	Shape      string
	Start, End int
}

// toolArgKeys are the argument-object keys a serialised call carries
// across the formats models actually emit: Anthropic's own wire name,
// OpenAI's, and the Hermes template's. Mirrors the grading side's
// agentgrade.toolCallArgKeys; kept separate because agentgrade imports
// this package and not the reverse.
var toolArgKeys = []string{"arguments", "parameters", "input"}

// toolWrapperOpen / toolWrapperClose are the template delimiters that
// surround a leaked call. They are not parsed for content — they are
// stripped, so removing the call does not leave scaffolding behind as
// visible prose.
var (
	toolWrapperOpen = []string{
		"<tool_call>", "<function_call>", "<tools>",
		"<|tool_call|>", "[TOOL_CALLS]", "<|python_tag|>",
		"```json", "```JSON", "```",
	}
	toolWrapperClose = []string{
		"</tool_call>", "</function_call>", "</tools>", "```",
	}
)

// toolNameDelimiters are the markers after which a model writes a bare
// tool name instead of putting it inside the JSON — granite4's
// `[TOOL_CALLS]Read{"file_path":…}{}` shape.
var toolNameDelimiters = []string{"[TOOL_CALLS]", "<|tool_call|>", "<|python_tag|>"}

// offeredTools indexes a request's tool definitions by name. The value
// is the tool's input_schema, used to type the arguments of dialects
// that serialise every value as text (see coerceBySchema).
type offeredTools map[string]json.RawMessage

func newOfferedTools(tools []AnthropicTool) offeredTools {
	if len(tools) == 0 {
		return nil
	}
	o := make(offeredTools, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			o[t.Name] = t.InputSchema
		}
	}
	return o
}

func (o offeredTools) has(name string) bool {
	_, ok := o[name]
	return ok
}

// recoverToolCall finds a tool call serialised into text and returns it
// as structured data, or reports false.
//
// It never returns a call whose name was not offered. That is the whole
// safety property: a model inventing a tool is a different defect
// (#322), one no parser can repair, and converting it would turn a
// visible failure into an invisible one.
//
// Shapes are tried most-specific first so a dialect that embeds JSON
// inside its own delimiters is attributed to the delimiter rather than
// to the JSON.
func recoverToolCall(text string, offered offeredTools) (recoveredCall, bool) {
	if len(offered) == 0 || text == "" {
		return recoveredCall{}, false
	}
	for _, find := range []func(string, offeredTools) (recoveredCall, bool){
		findXMLFunctionCall,
		findJSONObjectCall,
		findDelimitedCall,
	} {
		if c, ok := find(text, offered); ok {
			c.Start, c.End = expandOverWrappers(text, c.Start, c.End)
			return c, true
		}
	}
	return recoveredCall{}, false
}

// stripFragment removes a recovered call from the text it was found in,
// leaving the prose around it ("I'll check the contents of
// /etc/hostname…"). Whitespace-only remainders collapse to empty so the
// caller can drop the text block entirely.
func stripFragment(text string, c recoveredCall) string {
	out := text[:c.Start] + text[c.End:]
	if strings.TrimSpace(out) == "" {
		return ""
	}
	if c.End >= len(text) {
		// The call ran to the end, so the blank line before it was the
		// separator to the call and now separates nothing. Interior
		// spacing is left alone — there the whitespace still sits
		// between two pieces of prose.
		out = strings.TrimRight(out, " \t\r\n")
	}
	return out
}

// findXMLFunctionCall parses the Qwen3-Coder dialect:
//
//	<function=Bash>
//	<parameter=command>
//	cat /etc/hostname
//	</parameter>
//	</function>
//
// The opening <tool_call> is frequently missing (the engine's parser
// consumed it before failing on the body) and a stray closer frequently
// remains, so neither is required — expandOverWrappers cleans up
// whatever is actually there.
//
// Parameter bodies are raw text with the value on its own lines, so one
// leading and one trailing run of newlines is stripped; interior
// newlines are content and survive. Values are typed by the tool's
// schema, because the dialect itself carries no types.
func findXMLFunctionCall(text string, offered offeredTools) (recoveredCall, bool) {
	const (
		fnOpen    = "<function="
		fnClose   = "</function>"
		paramOpen = "<parameter="
		paramEnd  = "</parameter>"
	)
	start := strings.Index(text, fnOpen)
	if start < 0 {
		return recoveredCall{}, false
	}
	rest := text[start+len(fnOpen):]
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return recoveredCall{}, false
	}
	name := strings.TrimSpace(rest[:gt])
	if !offered.has(name) {
		return recoveredCall{}, false
	}

	body := rest[gt+1:]
	end := strings.Index(body, fnClose)
	if end < 0 {
		// No closer: the model was cut off mid-call. Everything after
		// the opener is the body; a truncated call still beats handing
		// the user raw template syntax.
		end = len(body)
	}
	args := map[string]string{}
	for scan := body[:end]; ; {
		i := strings.Index(scan, paramOpen)
		if i < 0 {
			break
		}
		scan = scan[i+len(paramOpen):]
		gt := strings.IndexByte(scan, '>')
		if gt < 0 {
			break
		}
		key := strings.TrimSpace(scan[:gt])
		scan = scan[gt+1:]
		valEnd := strings.Index(scan, paramEnd)
		if valEnd < 0 {
			if key != "" {
				args[key] = trimTemplateNewlines(scan)
			}
			break
		}
		if key != "" {
			args[key] = trimTemplateNewlines(scan[:valEnd])
		}
		scan = scan[valEnd+len(paramEnd):]
	}

	fragEnd := start + len(fnOpen) + gt + 1 + end
	if end < len(body) {
		fragEnd += len(fnClose)
	}
	return recoveredCall{
		Name:  name,
		Input: coerceBySchema(args, offered[name]),
		Shape: toolRecoveryXML,
		Start: start,
		End:   fragEnd,
	}, true
}

// findJSONObjectCall parses a bare {"name":…,"arguments":{…}} object,
// with or without a code fence or surrounding prose — the rc7 shape, and
// the one qwen2.5-coder produces on every trial.
//
// Both a string "name" and one of toolArgKeys are required. A model
// legitimately returning a {"name": …} record must not read as a leaked
// call, and requiring the pair is what keeps that from happening.
func findJSONObjectCall(text string, offered offeredTools) (recoveredCall, bool) {
	for _, span := range jsonObjectSpans(text) {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(text[span[0]:span[1]]), &m) != nil {
			continue
		}
		var name string
		raw, ok := m["name"]
		if !ok || json.Unmarshal(raw, &name) != nil || name == "" {
			continue
		}
		for _, k := range toolArgKeys {
			argsRaw, ok := m[k]
			if !ok {
				continue
			}
			if !offered.has(name) {
				// An unoffered name is a hallucination, not a
				// serialisation defect. Stop rather than keep scanning:
				// the response has exactly one leaked call in every
				// measured transcript, and hunting for a second invites
				// stitching a real name onto invented arguments.
				return recoveredCall{}, false
			}
			if !json.Valid(argsRaw) {
				return recoveredCall{}, false
			}
			return recoveredCall{
				Name:  name,
				Input: argsRaw,
				Shape: toolRecoveryJSON,
				Start: span[0],
				End:   span[1],
			}, true
		}
	}
	return recoveredCall{}, false
}

// findDelimitedCall parses a template delimiter followed by a bare tool
// name and a JSON argument object — granite4's
// `[TOOL_CALLS]Read{"file_path":"/etc/hostname"}{}`, where the name sits
// OUTSIDE any JSON and so is invisible to findJSONObjectCall.
func findDelimitedCall(text string, offered offeredTools) (recoveredCall, bool) {
	for _, marker := range toolNameDelimiters {
		i := strings.Index(text, marker)
		if i < 0 {
			continue
		}
		rest := text[i+len(marker):]
		consumed := len(marker)

		trimmed := strings.TrimLeft(rest, " \t\r\n")
		consumed += len(rest) - len(trimmed)
		name := leadingIdentifier(trimmed)
		if name == "" || !offered.has(name) {
			continue
		}
		consumed += len(name)
		afterName := trimmed[len(name):]

		objStart := strings.IndexByte(afterName, '{')
		if objStart < 0 {
			continue
		}
		objEnd, ok := matchJSONBrace(afterName, objStart)
		if !ok {
			continue
		}
		args := afterName[objStart : objEnd+1]
		if !json.Valid([]byte(args)) {
			continue
		}
		consumed += objEnd + 1

		// The template emits an empty object after the arguments (an
		// unused slot). Swallow it, but only when it is literally "{}":
		// anything else is content the model meant to write.
		if tail := strings.TrimLeft(afterName[objEnd+1:], " \t\r\n"); strings.HasPrefix(tail, "{}") {
			consumed += len(afterName[objEnd+1:]) - len(tail) + 2
		}
		return recoveredCall{
			Name:  name,
			Input: json.RawMessage(args),
			Shape: toolRecoveryDelimited,
			Start: i,
			End:   i + consumed,
		}, true
	}
	return recoveredCall{}, false
}

// coerceBySchema turns a dialect's untyped string arguments into JSON of
// the types the tool declares. Without it a `limit` the model wrote as
// `5` arrives as `"5"` and the client rejects the recovered call for
// failing its own schema validation — a recovery that does not survive
// the client is not a recovery.
//
// Only declared primitive types are converted, and only when the text
// actually parses as that type; everything else stays a string. Guessing
// beyond the schema would corrupt a legitimately numeric-looking string
// argument (a version, a zero-padded id).
func coerceBySchema(args map[string]string, schema json.RawMessage) json.RawMessage {
	types := schemaPropertyTypes(schema)
	out := make(map[string]json.RawMessage, len(args))
	for k, v := range args {
		out[k] = coerceValue(v, types[k])
	}
	// Keys are marshalled in sorted order, so the output is stable.
	encoded, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func coerceValue(v, declared string) json.RawMessage {
	switch declared {
	case "number", "integer":
		if _, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return json.RawMessage(strings.TrimSpace(v))
		}
	case "boolean":
		switch strings.TrimSpace(v) {
		case "true", "false":
			return json.RawMessage(strings.TrimSpace(v))
		}
	case "object", "array":
		if t := strings.TrimSpace(v); json.Valid([]byte(t)) {
			if (declared == "object" && strings.HasPrefix(t, "{")) ||
				(declared == "array" && strings.HasPrefix(t, "[")) {
				return json.RawMessage(t)
			}
		}
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// schemaPropertyTypes reads {"properties":{"k":{"type":"number"}}} out
// of a tool's input_schema. A missing or unparseable schema yields no
// types, which means every value stays a string — the safe direction.
func schemaPropertyTypes(schema json.RawMessage) map[string]string {
	if len(schema) == 0 {
		return nil
	}
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil
	}
	types := make(map[string]string, len(s.Properties))
	for k, p := range s.Properties {
		types[k] = p.Type
	}
	return types
}

// expandOverWrappers grows a fragment's bounds over the template
// delimiters and code fences immediately around it, so removing the call
// does not leave `<tool_call>` / ```` ``` ```` behind as visible text. It
// walks outward until neither side matches, because the dialects nest
// (a fenced JSON object inside a <tool_call> wrapper).
func expandOverWrappers(text string, start, end int) (int, int) {
	for range len(toolWrapperOpen) + len(toolWrapperClose) {
		newStart, grewLeft := expandLeft(text, start)
		newEnd, grewRight := expandRight(text, end)
		start, end = newStart, newEnd
		if !grewLeft && !grewRight {
			break
		}
	}
	return start, end
}

func expandLeft(text string, start int) (int, bool) {
	head := strings.TrimRight(text[:start], " \t\r\n")
	for _, open := range toolWrapperOpen {
		if strings.HasSuffix(head, open) {
			return len(head) - len(open), true
		}
	}
	return start, false
}

func expandRight(text string, end int) (int, bool) {
	tail := text[end:]
	trimmed := strings.TrimLeft(tail, " \t\r\n")
	for _, closer := range toolWrapperClose {
		if strings.HasPrefix(trimmed, closer) {
			return end + (len(tail) - len(trimmed)) + len(closer), true
		}
	}
	return end, false
}

// trimTemplateNewlines strips the line breaks the XML dialect puts
// around a parameter body without touching interior ones, which are
// content (a multi-line shell command, a file body).
func trimTemplateNewlines(s string) string {
	return strings.Trim(s, "\r\n")
}

// leadingIdentifier returns the tool-name-shaped prefix of s.
func leadingIdentifier(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			continue
		}
		return s[:i]
	}
	return s
}

// jsonObjectSpans returns the [start,end) bounds of the balanced {...}
// spans in s that parse as JSON objects, outermost first. It brace-scans
// rather than regexing because the objects nest and are routinely
// wrapped in prose or a fence; braces inside string literals are
// skipped, so a description containing "{" does not desynchronise it.
func jsonObjectSpans(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		end, ok := matchJSONBrace(s, i)
		if !ok {
			// Unbalanced from here on: no later opener can close either.
			break
		}
		if json.Valid([]byte(s[i : end+1])) {
			out = append(out, [2]int{i, end + 1})
			i = end // outermost only; nested objects come along inside
		}
	}
	return out
}

// matchJSONBrace returns the index of the '}' closing the '{' at start.
func matchJSONBrace(s string, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
