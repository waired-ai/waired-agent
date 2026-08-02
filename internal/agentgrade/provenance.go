package agentgrade

// Transports name the HTTP shape a run drove the gateway over. Recorded
// with the verdict because #409's tool-call recovery is two separate
// implementations — a whole-body parse on one path, a delta sieve that
// must decide what to withhold before the turn ends on the other — so
// "which path was measured" is part of what a verdict means.
const (
	TransportUnary  = "unary"
	TransportStream = "stream"
)

// agentRevision is the waired-agent commit the probe binary was built
// from, injected at link time by the Makefile:
//
//	-ldflags "-X github.com/waired-ai/waired-agent/internal/agentgrade.agentRevision=<sha>"
//
// It exists because FixtureRevision does not cover the GATEWAY. #409
// changed what the gateway does with a tool call the engine failed to
// parse: same model, same fixture, same engine, different answer — and
// nothing in a stored verdict marked the two generations apart, so the
// file silently mixed them.
//
// A hand-set flag would be the field that stops being updated, which is
// the argument FixtureRevision already makes for being a digest rather
// than a version string. A commit is stamped by the build, so it cannot
// drift from what was actually running.
//
// Empty when the probe was built without the flag (a plain `go test`).
// The importer refuses such a report rather than filing a verdict whose
// harness nobody can identify afterwards.
var agentRevision string

// AgentRevision is the commit the probe was built from, or "" when the
// build did not stamp one.
func AgentRevision() string { return agentRevision }
