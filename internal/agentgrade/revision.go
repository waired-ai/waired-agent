package agentgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// FixtureRevision identifies the exact probe input a verdict was
// measured against: a short digest over the tool set, the system
// prompt, the session context, and the case list.
//
// Verdicts are only comparable within one revision. The probe grades a
// model against a WEIGHT — #322's failure appears under a coding
// agent's load and not on a small request — so a fixture that grew or
// shrank between two runs makes their results incommensurable, and
// silently so: both say "pass". Recording the revision alongside each
// verdict is what makes the staleness visible instead.
//
// It is a digest rather than a hand-maintained version string because
// a hand-maintained one is exactly the thing that does not get bumped
// when somebody edits a tool description.
func FixtureRevision() (string, error) {
	revOnce.Do(func() {
		revValue, revErr = computeRevision()
	})
	return revValue, revErr
}

var (
	revOnce  sync.Once
	revValue string
	revErr   error
)

func computeRevision() (string, error) {
	h := sha256.New()

	tools, err := fixtureTools()
	if err != nil {
		return "", err
	}
	// Marshal the whole tool slice in declaration order: name,
	// description and schema all count, because all three are weight
	// the model has to carry.
	enc, err := json.Marshal(tools)
	if err != nil {
		return "", fmt.Errorf("agentgrade: hash tools: %w", err)
	}
	writeChunk(h, "tools", enc)
	writeChunk(h, "system", []byte(fixtureSystemPrompt))
	writeChunk(h, "context", []byte(fixtureProjectContext))

	for _, c := range Cases {
		// Why is documentation and does not change what the model sees,
		// so it is deliberately left out: editing a rationale must not
		// invalidate a run.
		writeChunk(h, "case", fmt.Appendf(nil, "%s\x00%t\x00%s", c.Name, c.WantToolCall, c.Prompt))
	}

	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// RequestBytes is the serialised size of the request the probe actually
// sends, for the first case.
//
// It is derived from BuildRequest rather than summed from the fixture's
// parts so that the drift canary compares against what goes on the wire.
// A separately-maintained number would be the thing that quietly stops
// matching.
func RequestBytes() (int, error) {
	if len(Cases) == 0 {
		return 0, fmt.Errorf("agentgrade: no probe cases defined")
	}
	req, err := BuildRequest(fixtureModel, Cases[0])
	if err != nil {
		return 0, err
	}
	enc, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("agentgrade: marshal request: %w", err)
	}
	return len(enc), nil
}

// writeChunk length-prefixes each field so concatenation cannot alias:
// without it, moving a sentence from the system prompt into the session
// context would leave the digest unchanged.
func writeChunk(h io.Writer, label string, b []byte) {
	_, _ = fmt.Fprintf(h, "%s:%d:", label, len(b))
	_, _ = h.Write(b)
}
