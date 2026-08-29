package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestComputeStickyID_HeaderPreferred(t *testing.T) {
	h := http.Header{}
	h.Set(stickyHeader, "session-abc-123")
	got := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), StickyIdentity{})
	if got != "session-abc-123" {
		t.Errorf("header ID should win; got %q", got)
	}
}

// TestComputeStickyID_HeaderOutranksIdentity keeps the opt-in header at
// the head of the chain: a client that threads its own conversation id
// has said something no inference from the body can improve on.
func TestComputeStickyID_HeaderOutranksIdentity(t *testing.T) {
	h := http.Header{}
	h.Set(stickyHeader, "conv-7")
	id := StickyIdentity{
		Metadata:     json.RawMessage(`{"user_id":"whoever"}`),
		FirstMessage: json.RawMessage(`{"role":"user","content":"hi"}`),
	}
	if got := ComputeStickyID(h, []byte(`{"model":"m"}`), id); got != "conv-7" {
		t.Errorf("header should outrank identity material; got %q", got)
	}
}

func TestComputeStickyID_HeaderSanitised(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc def", "abcdef"},
		{"abc\nxyz", "abcxyz"},
		{"a!@#$%^&*()b", "ab"},
		// Verifies length truncation to 32 chars.
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"safe-id_42", "safe-id_42"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			h := http.Header{}
			h.Set(stickyHeader, c.in)
			got := ComputeStickyID(h, nil, StickyIdentity{})
			if got != c.want {
				t.Errorf("sanitise(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- the identity rung (waired-agent#1125) ---------------------------
//
// The four tests below are the measured defect, turned into a guard.
// The fixture reproduces the SHAPE of what a real `claude` CLI sends —
// it is not a capture, because this repository is public and a real
// body carries the client's device and session identifiers.

// claudeShapedBody builds a request body with the wire shape measured
// in waired-agent#1125: `model` is the FIRST key, messages[0] opens
// with a long block that is byte-identical across sessions started in
// one repository, the distinguishing bytes sit far past 1 KiB, and
// `metadata` sits near the end.
func claudeShapedBody(t *testing.T, model, prompt, userID string) []byte {
	t.Helper()
	// 6,572 bytes: the measured size of the shared system-reminder block
	// that opens messages[0] in every session started in one repository.
	shared := strings.Repeat("system-reminder ", 6572/16)
	body := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":%q},{"type":"text","text":%q}]}],`+
			`"system":"you are a coding agent","tools":[],"metadata":{"user_id":%q},"max_tokens":32000}`,
		model, shared, prompt, userID)
	if !json.Valid([]byte(body)) {
		t.Fatalf("fixture is not valid JSON")
	}
	return []byte(body)
}

// stickyForBody runs the real Anthropic-surface path: unmarshal the
// body the way the handler does, then ask for an id.
func stickyForBody(t *testing.T, body []byte) string {
	t.Helper()
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return ComputeStickyID(http.Header{}, body, stickyIdentityFromAnthropic(req))
}

// TestComputeStickyID_TwoSessionsInOneRepositoryDiverge is the first
// half of waired-agent#1125. Measured: two `claude` sessions opened in
// one repository first differ at byte 6,628 — 6.5x past the 1 KiB
// window — so the prefix hash collapsed them into one conversation and
// bound both to the same peer, where they evicted each other's KV.
func TestComputeStickyID_TwoSessionsInOneRepositoryDiverge(t *testing.T) {
	a := stickyForBody(t, claudeShapedBody(t, "claude-waired-auto", "reply with the single word one", "sess-A"))
	b := stickyForBody(t, claudeShapedBody(t, "claude-waired-auto", "reply with the single word two", "sess-B"))
	if a == b {
		t.Errorf("two sessions in one repository must not share a sticky id; both are %q", a)
	}
}

// TestComputeStickyID_SessionsDivergeOnTheirPromptAlone narrows the one
// above: even with the SAME user id, the prompt that sits past the
// 1 KiB window has to separate them. This is the assertion the old
// prefix hash failed.
func TestComputeStickyID_SessionsDivergeOnTheirPromptAlone(t *testing.T) {
	a := stickyForBody(t, claudeShapedBody(t, "claude-waired-auto", "reply with the single word one", "sess-A"))
	b := stickyForBody(t, claudeShapedBody(t, "claude-waired-auto", "reply with the single word two", "sess-A"))
	if a == b {
		t.Errorf("bytes past the 1 KiB window must still separate two conversations; both are %q", a)
	}
}

// TestComputeStickyID_ModelPickDoesNotSplitAConversation is the second
// half of waired-agent#1125. `model` is the first key on the wire, so
// every byte-prefix window carries it — and this product delivers
// claude-waired-auto / -peer / -local / -cloud to the client AS MODEL
// IDS (#830, #1036). Under the prefix hash, picking a different entry
// in /model dropped the binding and bought a full re-prefill.
func TestComputeStickyID_ModelPickDoesNotSplitAConversation(t *testing.T) {
	auto := stickyForBody(t, claudeShapedBody(t, "claude-waired-auto", "same conversation", "sess-A"))
	peer := stickyForBody(t, claudeShapedBody(t, "claude-waired-peer", "same conversation", "sess-A"))
	if auto != peer {
		t.Errorf("a /model pick must not change the conversation id; got %q vs %q", auto, peer)
	}
}

// TestComputeStickyID_TurnsOfOneSessionAgree is the other measured
// direction: turn 2 of a session carries a much larger body than turn 1
// (90,215 B vs 127,210 B measured) while messages[0] stays byte-for-byte
// identical and metadata.user_id survives a --continue.
func TestComputeStickyID_TurnsOfOneSessionAgree(t *testing.T) {
	turn1 := claudeShapedBody(t, "claude-waired-auto", "start the task", "sess-A")
	// Turn 2: the assistant reply and the next user turn are appended, so
	// the body grows while messages[0] does not change.
	var req AnthropicRequest
	if err := json.Unmarshal(turn1, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req.Messages = append(req.Messages,
		AnthropicMessage{Role: "assistant", Content: json.RawMessage(`"on it"`)},
		AnthropicMessage{Role: "user", Content: json.RawMessage(`"` + strings.Repeat("more work ", 4000) + `"`)})
	turn2, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(turn2) <= len(turn1) {
		t.Fatalf("fixture: turn 2 should be the larger body (%d vs %d)", len(turn2), len(turn1))
	}
	if a, b := stickyForBody(t, turn1), stickyForBody(t, turn2); a != b {
		t.Errorf("two turns of one session must share a sticky id; got %q vs %q", a, b)
	}
}

// TestComputeStickyID_IdentityNeedsBothHalves pins why the rung mixes
// the client's user id with its first message. Each half alone
// over-merges, in opposite directions.
func TestComputeStickyID_IdentityNeedsBothHalves(t *testing.T) {
	firstA := json.RawMessage(`{"role":"user","content":"plan the migration"}`)
	firstB := json.RawMessage(`{"role":"user","content":"write the changelog"}`)
	userA := json.RawMessage(`{"user_id":"person-1"}`)
	userB := json.RawMessage(`{"user_id":"person-2"}`)
	h := http.Header{}
	body := []byte(`{"model":"m"}`)

	// One person, two conversations. metadata.user_id is specified as
	// identifying the PERSON, so a client that sends a stable per-user
	// string would collapse every conversation onto one peer if the user
	// id were the whole rung.
	sameUser := ComputeStickyID(h, body, StickyIdentity{Metadata: userA, FirstMessage: firstA})
	otherConv := ComputeStickyID(h, body, StickyIdentity{Metadata: userA, FirstMessage: firstB})
	if sameUser == otherConv {
		t.Errorf("one person's two conversations must not share an id; both are %q", sameUser)
	}

	// Two people opening with the same words. The first message alone
	// would merge them.
	otherPerson := ComputeStickyID(h, body, StickyIdentity{Metadata: userB, FirstMessage: firstA})
	if sameUser == otherPerson {
		t.Errorf("two people with the same opening must not share an id; both are %q", sameUser)
	}
}

// TestComputeStickyID_IdentityOutranksTheBodyPrefix confirms the rung
// order: any identity material at all beats the byte prefix, so two
// bodies that share their first kilobyte still separate.
func TestComputeStickyID_IdentityOutranksTheBodyPrefix(t *testing.T) {
	h := http.Header{}
	shared := []byte(`{"model":"m","messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 200) + `null]}`)
	a := ComputeStickyID(h, shared, StickyIdentity{FirstMessage: json.RawMessage(`{"role":"user","content":"a"}`)})
	b := ComputeStickyID(h, shared, StickyIdentity{FirstMessage: json.RawMessage(`{"role":"user","content":"b"}`)})
	if a == b {
		t.Errorf("identity material must outrank the body prefix; both are %q", a)
	}
	plain := ComputeStickyID(h, shared, StickyIdentity{})
	if plain == a || plain == b {
		t.Errorf("the body rung must not collide with the identity rung; %q", plain)
	}
}

// TestStickyIdentity_UserIDIsNoClaimWhenUnusable keeps an absent, null
// or wrongly-typed member from reading as an empty user id — the same
// "absence is no claim" rule the residency and host-speed fields hold.
func TestStickyIdentity_UserIDIsNoClaimWhenUnusable(t *testing.T) {
	cases := []struct {
		name string
		id   StickyIdentity
		want string
	}{
		{"absent", StickyIdentity{}, ""},
		{"metadata null", StickyIdentity{Metadata: json.RawMessage(`null`)}, ""},
		{"metadata without user_id", StickyIdentity{Metadata: json.RawMessage(`{"other":1}`)}, ""},
		{"user_id not a string", StickyIdentity{Metadata: json.RawMessage(`{"user_id":42}`)}, ""},
		{"metadata malformed", StickyIdentity{Metadata: json.RawMessage(`{`)}, ""},
		{"metadata wins", StickyIdentity{Metadata: json.RawMessage(`{"user_id":"m"}`), User: json.RawMessage(`"u"`)}, "m"},
		{"openai user", StickyIdentity{User: json.RawMessage(`"u"`)}, "u"},
		{"openai user not a string", StickyIdentity{User: json.RawMessage(`{"id":"u"}`)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.id.userID(); got != c.want {
				t.Errorf("userID() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestComputeStickyID_UnusableIdentityFallsThroughToTheBody makes the
// degradation explicit: a client that offers nothing usable gets the
// historical behaviour rather than one shared id.
func TestComputeStickyID_UnusableIdentityFallsThroughToTheBody(t *testing.T) {
	h := http.Header{}
	empty := StickyIdentity{Metadata: json.RawMessage(`{"other":1}`)}
	a := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), empty)
	b := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), StickyIdentity{})
	if a != b {
		t.Errorf("unusable identity must fall through to the body rung; got %q vs %q", a, b)
	}
	if a == "" {
		t.Error("the body rung should still have produced an id")
	}
}

// --- the OpenAI surface ----------------------------------------------

func TestStickyIdentityFromOpenAIBody(t *testing.T) {
	raw, err := decodeJSONObject([]byte(
		`{"model":"m","user":"person-1","messages":[{"role":"user","content":"first"},{"role":"user","content":"second"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := stickyIdentityFromOpenAIBody(raw)
	if got := id.userID(); got != "person-1" {
		t.Errorf("userID() = %q, want person-1", got)
	}
	if got := string(id.FirstMessage); got != `{"role":"user","content":"first"}` {
		t.Errorf("FirstMessage = %s, want only messages[0]", got)
	}
}

func TestFirstJSONArrayElement(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"object elements", `[{"a":1},{"b":2}]`, `{"a":1}`},
		{"whitespace", "[\n  {\"a\":1}\n]", `{"a":1}`},
		{"single element", `[{"a":1}]`, `{"a":1}`},
		{"empty array", `[]`, ``},
		{"not an array", `{"a":1}`, ``},
		{"malformed", `[{"a":`, ``},
		{"empty input", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(firstJSONArrayElement(json.RawMessage(c.in))); got != c.want {
				t.Errorf("firstJSONArrayElement(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- the body rung, unchanged ----------------------------------------

// TestComputeStickyID_BodyHashStability documents the last-resort
// fallback: identical request bodies must produce identical sticky
// IDs across processes, so two requests from the same conversation
// pin to the same peer even without the client header.
func TestComputeStickyID_BodyHashStability(t *testing.T) {
	h := http.Header{}
	body := []byte(`{"messages":[{"role":"system","content":"you are a senior engineer working on the waired repo..."}]}`)
	a := ComputeStickyID(h, body, StickyIdentity{})
	b := ComputeStickyID(h, body, StickyIdentity{})
	if a != b {
		t.Errorf("identical bodies must hash identically; got %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("body-hash ID should be 16 hex chars; got %d (%q)", len(a), a)
	}
	if !isHex(a) {
		t.Errorf("body-hash ID should be hex; got %q", a)
	}
}

// TestComputeStickyID_BodyDifferentBodiesDiverge confirms two
// different prompts produce different IDs — otherwise affinity
// would over-stick conversations together.
func TestComputeStickyID_BodyDifferentBodiesDiverge(t *testing.T) {
	h := http.Header{}
	a := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), StickyIdentity{})
	b := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"world"}]}`), StickyIdentity{})
	if a == b {
		t.Errorf("different bodies should produce different IDs; both are %q", a)
	}
}

// TestComputeStickyID_PrefixOnlyMatters captures what the LAST rung
// still does: only the first 1 KiB contributes. It is why that rung is
// the last one — see TestComputeStickyID_TwoSessionsInOneRepositoryDiverge
// for the conversations it could not tell apart.
func TestComputeStickyID_PrefixOnlyMatters(t *testing.T) {
	h := http.Header{}
	base := strings.Repeat("x", stickyPrefixBytes) // exactly fills the prefix
	a := ComputeStickyID(h, []byte(base+"AAA"), StickyIdentity{})
	b := ComputeStickyID(h, []byte(base+"BBB"), StickyIdentity{})
	if a != b {
		t.Errorf("bytes beyond the prefix should not affect ID; got %q vs %q", a, b)
	}
}

// TestComputeStickyID_EmptyInputs returns empty string when neither
// header nor body offer a signal. The Selector treats "" as "no
// affinity hint" and routes purely by score.
func TestComputeStickyID_EmptyInputs(t *testing.T) {
	h := http.Header{}
	if got := ComputeStickyID(h, nil, StickyIdentity{}); got != "" {
		t.Errorf("empty inputs should produce empty ID; got %q", got)
	}
	if got := ComputeStickyID(h, []byte{}, StickyIdentity{}); got != "" {
		t.Errorf("zero-length body should produce empty ID; got %q", got)
	}
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
