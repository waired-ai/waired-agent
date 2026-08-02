package gateway

import (
	"strings"
	"testing"
)

// pushAll streams text through the sieve one delta at a time and returns
// everything the sieve released before Finish.
func pushAll(s *toolTextSieve, deltas ...string) string {
	var out strings.Builder
	for _, d := range deltas {
		out.WriteString(s.Push(d))
	}
	return out.String()
}

// Product contract: ordinary prose is released as it arrives. The whole
// point of the sieve over a simple "buffer the turn" fix is that
// time-to-first-byte and progressive display survive for the responses
// that have nothing wrong with them — which is nearly all of them.
func TestToolTextSieve_ordinaryProsePassesThrough(t *testing.T) {
	s := newToolTextSieve(readTools())
	deltas := []string{"The ", "file ", "contains ", "one ", "line."}
	got := pushAll(s, deltas...)
	if want := strings.Join(deltas, ""); got != want {
		t.Errorf("released %q, want %q streamed through unchanged", got, want)
	}
	tail, _, ok := s.Finish()
	if ok || tail != "" {
		t.Errorf("Finish = (%q, ok=%v), want nothing held", tail, ok)
	}
}

// Product contract: a code block is NOT held. Coding agents emit fenced
// code constantly, and stalling every fence to catch a rare leaked call
// would be a worse trade than the defect.
func TestToolTextSieve_codeFenceIsNotWithheld(t *testing.T) {
	s := newToolTextSieve(readTools())
	deltas := []string{"Here:\n", "```go\n", "x := map[string]int{}\n", "```\n", "Done."}
	got := pushAll(s, deltas...)
	if want := strings.Join(deltas, ""); got != want {
		t.Errorf("released %q, want the fenced block streamed through unchanged", got)
	}
}

// Product contract: the sentinel is matched across delta boundaries. An
// engine streams roughly a token at a time, so "<function=Bash>" arrives
// in pieces — a scanner that only looked at whole deltas would emit the
// first half as text and never recover anything.
func TestToolTextSieve_sentinelSplitAcrossDeltas(t *testing.T) {
	s := newToolTextSieve(readTools())
	released := pushAll(s,
		"I'll run it.\n\n", "<func", "tion=", "Bash>\n",
		"<parameter=", "command>\nls\n</parameter>\n", "</function>",
	)
	if released != "I'll run it.\n\n" {
		t.Errorf("released %q, want only the prose before the call", released)
	}
	tail, c, ok := s.Finish()
	if !ok {
		t.Fatalf("no call recovered; tail=%q", tail)
	}
	if c.Name != "Bash" {
		t.Errorf("tool = %q, want Bash", c.Name)
	}
	if tail != "" {
		t.Errorf("tail = %q, want the fragment removed entirely", tail)
	}
}

// Product contract: a held tail that turns out NOT to be a call is
// released verbatim. Withholding text is a bet, and losing the bet must
// cost latency only, never content.
func TestToolTextSieve_falsePositiveIsReleasedVerbatim(t *testing.T) {
	s := newToolTextSieve(readTools())
	// Opens with the JSON sentinel, but names a tool that was never
	// offered — so it stays exactly what the model wrote.
	const prose = `Consider {"name": "Deploy", "arguments": {"env": "prod"}} as an example.`
	released := pushAll(s, prose)
	tail, _, ok := s.Finish()
	if ok {
		t.Fatal("recovered a call from an unoffered name")
	}
	if released+tail != prose {
		t.Errorf("released+tail = %q, want %q byte-for-byte", released+tail, prose)
	}
}

// Product contract: the JSON sentinel fires on either key order. Every
// measured transcript put "name" first, but nothing in the format
// requires it.
func TestToolTextSieve_jsonSentinelBothKeyOrders(t *testing.T) {
	for _, body := range []string{
		`{"name":"Read","arguments":{"file_path":"/etc/hostname"}}`,
		`{"arguments":{"file_path":"/etc/hostname"},"name":"Read"}`,
	} {
		s := newToolTextSieve(readTools())
		released := pushAll(s, "Reading.\n", body)
		if released != "Reading.\n" {
			t.Errorf("released %q for %s, want only the prose", released, body)
		}
		if _, c, ok := s.Finish(); !ok || c.Name != "Read" {
			t.Errorf("recovery for %s = (%q, ok=%v), want Read", body, c.Name, ok)
		}
	}
}

// Product contract: Flush releases without recovering. It is what the
// stream path uses when the engine DID emit structured tool_calls — its
// parser worked, so anything call-shaped left in the text is the model's
// own prose and must not be converted into a second call.
func TestToolTextSieve_flushDoesNotRecover(t *testing.T) {
	s := newToolTextSieve(readTools())
	released := pushAll(s, "See ", `{"name":"Read","arguments":{"file_path":"/x"}}`)
	if got := released + s.Flush(); got != `See {"name":"Read","arguments":{"file_path":"/x"}}` {
		t.Errorf("Flush produced %q, want the text unchanged", got)
	}
	if tail, _, ok := s.Finish(); ok || tail != "" {
		t.Errorf("Finish after Flush = (%q, %v), want empty", tail, ok)
	}
}

// Product contract: with no tools offered the sieve is a pass-through,
// so a request that cannot have tool calls pays nothing for this feature.
func TestToolTextSieve_noToolsIsPassThrough(t *testing.T) {
	s := newToolTextSieve(nil)
	if got := s.Push(fencedJSONTranscript); got != fencedJSONTranscript {
		t.Errorf("released %q, want the input unchanged", got)
	}
	if tail, _, ok := s.Finish(); ok || tail != "" {
		t.Errorf("Finish = (%q, %v), want nothing held", tail, ok)
	}
}

// Records today's behaviour: past toolSieveMaxHold the sieve gives up
// and releases, so a false-positive sentinel in a very long turn cannot
// pin the whole response in memory or blank the client for its duration.
// The turn then behaves exactly as it did before #409.
func TestToolTextSieve_releasesPastTheHoldCap(t *testing.T) {
	s := newToolTextSieve(readTools())
	released := pushAll(s, `{"name":"Read","arguments":{"file_path":"`)
	if released != "" {
		t.Fatalf("released %q before the cap, want everything held", released)
	}
	overflow := pushAll(s, strings.Repeat("x", toolSieveMaxHold+1))
	if overflow == "" {
		t.Error("nothing released past the hold cap; the sieve would buffer without bound")
	}
}

// Records today's behaviour of the marker scanner: a partial sentinel at
// the tail is held, but only the partial — everything before it is
// already safe to release.
func TestSuspicionStart_partialTailIsNotDefinite(t *testing.T) {
	i, definite := suspicionStart([]byte("hello <tool_c"))
	if i != len("hello ") {
		t.Errorf("offset = %d, want %d", i, len("hello "))
	}
	if definite {
		t.Error("a partial sentinel must not be definite; the next delta may resolve it to prose")
	}

	i, definite = suspicionStart([]byte("hello <tool_call>"))
	if i != len("hello ") || !definite {
		t.Errorf("complete sentinel = (%d, %v), want (%d, true)", i, definite, len("hello "))
	}

	if i, _ := suspicionStart([]byte("nothing suspicious here")); i != len("nothing suspicious here") {
		t.Errorf("offset = %d, want the whole buffer releasable", i)
	}
}
