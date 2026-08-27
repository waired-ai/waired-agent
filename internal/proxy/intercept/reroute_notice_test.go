package intercept

import (
	"io"
	"strings"
	"testing"
)

// Minimal Anthropic SSE fixtures. Each ends with the terminal message_delta /
// message_stop so the injector has a boundary to splice before.
const sseMessageStart = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"

const sseMessageTail = "event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func textBlock(index int, text string) string {
	return "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":` + itoa(index) + `,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":` + itoa(index) + `,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":` + itoa(index) + `}` + "\n\n"
}

func thinkingBlock(index int) string {
	return "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":` + itoa(index) + `,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":` + itoa(index) + `}` + "\n\n"
}

func toolUseBlock(index int) string {
	return "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":` + itoa(index) + `,"content_block":{"type":"tool_use","id":"tu_1","name":"grep","input":{}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":` + itoa(index) + `}` + "\n\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const notice = "__ROUTED__"

func inject(t *testing.T, r io.Reader) string {
	t.Helper()
	rc := newRerouteNoticeInjector(io.NopCloser(r), notice)
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read injector: %v", err)
	}
	_ = rc.Close()
	return string(out)
}

func TestRerouteNoticeInjector(t *testing.T) {
	cases := []struct {
		name      string
		sse       string
		wantIndex string // the index the injected block must use
	}{
		{"pure text (1 block)", sseMessageStart + textBlock(0, "hi") + sseMessageTail, `"index":1`},
		{"thinking + text (2 blocks)", sseMessageStart + thinkingBlock(0) + textBlock(1, "hi") + sseMessageTail, `"index":2`},
		{"zero content blocks", sseMessageStart + sseMessageTail, `"index":0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := inject(t, strings.NewReader(tc.sse))
			if !strings.Contains(out, notice) {
				t.Fatalf("notice not injected:\n%s", out)
			}
			if !strings.Contains(out, tc.wantIndex) {
				t.Errorf("injected block missing %s:\n%s", tc.wantIndex, out)
			}
			// The notice must land BEFORE the terminal message_delta.
			if strings.Index(out, notice) > strings.Index(out, "event: message_delta") {
				t.Errorf("notice injected after message_delta:\n%s", out)
			}
			// The pre-existing stream is preserved intact around the splice.
			if !strings.Contains(out, "event: message_stop") {
				t.Errorf("terminal events lost:\n%s", out)
			}
		})
	}
}

// A tool_use response is structured output the orchestrator parses; the
// injector must forward it byte-for-byte and never splice prose.
func TestRerouteNoticeInjector_ToolUseUntouched(t *testing.T) {
	in := sseMessageStart + textBlock(0, "let me look") + toolUseBlock(1) + sseMessageTail
	out := inject(t, strings.NewReader(in))
	if out != in {
		t.Errorf("tool_use stream was modified.\n--- in ---\n%s\n--- out ---\n%s", in, out)
	}
	if strings.Contains(out, notice) {
		t.Error("notice injected into a tool_use response")
	}
}

// No terminal message_delta (truncated stream) => fail-open, byte-identical.
func TestRerouteNoticeInjector_NoBoundaryFailsOpen(t *testing.T) {
	in := sseMessageStart + textBlock(0, "partial")
	out := inject(t, strings.NewReader(in))
	if out != in {
		t.Errorf("truncated stream not forwarded verbatim.\n--- in ---\n%s\n--- out ---\n%s", in, out)
	}
}

// oneByteReader returns at most one byte per Read, exercising the injector's
// line reassembly across chunk boundaries.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestRerouteNoticeInjector_PartialChunks(t *testing.T) {
	in := sseMessageStart + textBlock(0, "hi") + sseMessageTail
	whole := inject(t, strings.NewReader(in))
	drip := inject(t, oneByteReader{strings.NewReader(in)})
	if whole != drip {
		t.Errorf("byte-dripped output differs from whole-read output.\n--- whole ---\n%s\n--- drip ---\n%s", whole, drip)
	}
}

// buildRerouteNotice says what actually happened, per reason. The wording is
// the product string the docs quote verbatim, so this is where a change to it
// has to be made deliberately.
//
// waired-agent#1040 added the two peer-answered reasons: past its grace
// period the gateway asks the peer whether it is still working, so a turn can
// now leave the mesh because the peer said it had stopped — which "returned
// no response" would describe wrongly.
func TestBuildRerouteNotice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		class    string
		localErr string
		peer     string
		budgetMs string
		want     []string
		absent   []string
	}{
		{
			name:  "a peer that reported it had stopped working",
			class: classMain, localErr: localErrPeerStoppedServing,
			peer: "sv-mag", budgetMs: "180000",
			want: []string{"this turn", "sv-mag", "stopped working on it", "after 3 minutes"},
			// It did answer — calling that "no response" is the account
			// this reason exists to replace.
			absent: []string{"no response"},
		},
		{
			name:  "a peer that went silent",
			class: classSub, localErr: localErrPeerUnreachable,
			peer: "sv-mag", budgetMs: "90000",
			want:   []string{"this subagent turn", "sv-mag", "stopped answering", "after 90s"},
			absent: []string{"no response"},
		},
		{
			// Unchanged shipped copy: the flat deadline still reads as a
			// timeout, because that is all it observed.
			name:  "the flat deadline still reads as a timeout",
			class: classMain, localErr: localErrPeerTTFBTimeout,
			peer: "sv-mag", budgetMs: "60000",
			want: []string{"returned no response", "within 60s", "sv-mag"},
		},
		{
			// A reason with no peer name has no machine to talk about, so
			// it falls through to the generic sentence rather than saying
			// "a mesh peer ()".
			name:  "a peer-answered reason with no peer named falls through",
			class: classMain, localErr: localErrPeerStoppedServing,
			peer: "", budgetMs: "180000",
			want:   []string{"local/mesh serving", "was unavailable"},
			absent: []string{"stopped working on it"},
		},
		{
			// An unparseable elapsed figure drops the clause instead of
			// rendering an empty one.
			name:  "an unreadable elapsed figure is simply not mentioned",
			class: classMain, localErr: localErrPeerStoppedServing,
			peer: "sv-mag", budgetMs: "",
			want:   []string{"sv-mag", "stopped working on it"},
			absent: []string{" after "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRerouteNotice(tc.class, tc.localErr, tc.peer, tc.budgetMs)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice is missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("notice should not contain %q:\n%s", absent, got)
				}
			}
			if !strings.Contains(got, "waired claude route") {
				t.Errorf("every notice names the way out:\n%s", got)
			}
		})
	}
}
