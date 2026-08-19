package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// A round where nothing answered is the shape a host with a dead overlay
// produces on every request it ever makes. The sentinel alone told the
// operator the probes went unanswered and stopped there, which reads as
// a fact about the peers when it is a fact about this machine (#849).

func TestUnansweredMeshError_NamesThePeersAndKeepsTheSentinel(t *testing.T) {
	got := unansweredMeshError(probedSelection{
		cands: []router.Candidate{
			{PeerID: "dev_a", ExecutionMode: "remote"},
			{PeerID: "dev_b", ExecutionMode: "remote"},
		},
		probeResults: []router.ProbeResult{
			{Outcome: router.ProbeTransportError, Err: errors.New("dial: no route to host")},
			{Outcome: router.ProbeAuthError, Err: errors.New("401")},
		},
	})

	if !errors.Is(got, router.ErrPeersDidNotAnswer) {
		t.Fatalf("sentinel lost: %v", got)
	}
	// Chaining both would make the error satisfy two sentinels, and
	// selectionErrorReason / selectionStatus test the overloaded one
	// first — the request would then be reported as the wrong failure.
	if errors.Is(got, router.ErrAllPeersOverloaded) {
		t.Errorf("error also matches the capacity sentinel: %v", got)
	}
	msg := got.Error()
	for _, want := range []string{"dev_a", "dev_b", "no answer", "rejected our identity", "from this computer"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %q", want, msg)
		}
	}
}

// Spec §8.5 / #739: this string is written verbatim into the 503 body
// the client reads, so a stranger's real device identifier must never
// appear in it.
func TestUnansweredMeshError_UsesTheDisplayIdentifier(t *testing.T) {
	got := unansweredMeshError(probedSelection{
		cands: []router.Candidate{
			{PeerID: "dev_foreign00000001", PeerDisplayID: "public-7", ExecutionMode: "remote"},
		},
		probeResults: []router.ProbeResult{{Outcome: router.ProbeTransportError}},
	})

	msg := got.Error()
	if !strings.Contains(msg, "public-7") {
		t.Errorf("pseudonym not used: %q", msg)
	}
	if strings.Contains(msg, "dev_foreign00000001") {
		t.Errorf("real device identifier leaked into the client-visible body: %q", msg)
	}
}

// Nothing to name is not a reason to invent a shape: the bare sentinel
// is still the honest answer.
func TestUnansweredMeshError_FallsBackToTheBareSentinel(t *testing.T) {
	if got := unansweredMeshError(probedSelection{}); got != router.ErrPeersDidNotAnswer {
		t.Errorf("got %v, want the bare sentinel", got)
	}
}

// probeResults indexes into cands. A short cands slice must truncate
// rather than pair a result with the wrong peer's name.
func TestUnansweredMeshError_DoesNotMisattributeAResult(t *testing.T) {
	got := unansweredMeshError(probedSelection{
		cands: []router.Candidate{{PeerID: "dev_a", ExecutionMode: "remote"}},
		probeResults: []router.ProbeResult{
			{Outcome: router.ProbeTransportError},
			{Outcome: router.ProbeAuthError},
		},
	})

	if msg := got.Error(); strings.Contains(msg, "rejected our identity") {
		t.Errorf("a result with no candidate was attributed to one: %q", msg)
	}
}
