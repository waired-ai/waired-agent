package runtime

// Converging the bundled engine onto the pin (#826).
//
// The agent serves only with the engine waired installed, and it accepts
// only the exact pinned version — #489 decided the first and its
// consequence line records the second. Those two together mean that every
// time OllamaPinnedVersion moves, every host that took the agent update is
// left with an engine the agent will not serve with, until somebody runs
// `waired runtimes install ollama` by hand. v0.0.3-rc2 is the case that
// made it concrete: the pin moved to 0.32.13 because 0.31.1 cannot pull
// qwen3.8 at all, so the model shipped and was unusable on every updated
// host.
//
// The policy is one sentence: converge an engine that is already there,
// never create one that is not. `waired init` is the only thing that
// installs an engine (#138), because it is the only thing that asks
// whether this computer should run models; an update must not answer that
// question by downloading 1.4 GB onto a host that said no.

import (
	"context"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/version"
)

// OllamaConvergeFacts is what the decision is made from. Facts, not
// handles: the decision is pure so the three callers — the CLI the
// installer scripts invoke, the daemon's reconcile, and tests — cannot
// each grow their own variant of the rule.
type OllamaConvergeFacts struct {
	// Installed reports whether a bundled engine binary is present and
	// executable under the waired-owned runtimes directory.
	Installed bool

	// Version is what that binary reports, or "" when it cannot be read.
	// Note this is the BINARY's version and not a running server's: a
	// stopped engine has one, and until #826 the product could not read
	// it (see hardware.ParseEngineVersion).
	Version string

	// Pin is the version this build serves with, normally
	// OllamaPinnedVersion.
	Pin string
}

// OllamaConvergeDecision is the answer, with the reason it was reached.
// A reason string rather than a bare bool for the same reason the catalog's
// InternalOnly carries one: this is printed at the end of an update, and
// "nothing to do" and "cannot tell" must not look alike.
type OllamaConvergeDecision struct {
	Install bool
	Reason  string
}

// DecideOllamaConverge is the whole policy.
func DecideOllamaConverge(f OllamaConvergeFacts) OllamaConvergeDecision {
	if !f.Installed {
		return OllamaConvergeDecision{
			Reason: "no bundled engine on this host; `waired init` is what installs one",
		}
	}
	if f.Version == "" {
		return OllamaConvergeDecision{
			Install: true,
			Reason:  fmt.Sprintf("the bundled engine does not report a version; reinstalling the pin %s", f.Pin),
		}
	}
	// Equality through the version parser rather than string comparison:
	// "v0.32.13" and "0.32.13" are one release, and a converge that fires
	// on the spelling would download 1.4 GB to change nothing.
	if cmp, ok := version.Compare(f.Version, f.Pin); ok && cmp == 0 {
		return OllamaConvergeDecision{
			Reason: fmt.Sprintf("bundled engine is already at the pin %s", f.Pin),
		}
	}
	// Deliberately not "older than": a pin can move backwards when an
	// engine release is reverted, and the agent's rule is exact match, so
	// converging down is as necessary as converging up.
	return OllamaConvergeDecision{
		Install: true,
		Reason:  fmt.Sprintf("bundled engine is %s, pin is %s", f.Version, f.Pin),
	}
}

// OllamaConvergeDeps are the seams, exported because the two real callers
// install differently and the ORCHESTRATION is what must not be written
// twice. The CLI's Install draws a progress bar, overlays ROCm on AMD and
// hands the state dir back to the service user; the daemon's just fetches.
// Both go through the same probe → decide → install sequence below.
//
// Each seam takes and returns what the real thing does, so a fake cannot
// make an untestable case disappear — Probe in particular receives the
// path it was asked about, which is how the test pins that the BUNDLED
// binary is probed rather than whatever is on PATH.
type OllamaConvergeDeps struct {
	// Present reports whether a bundled engine binary is installed.
	Present func() bool
	// BinaryPath is that binary, for Probe.
	BinaryPath func() string
	// Probe runs `<path> --version`; hardware.EngineVersionAt is the one
	// the product uses, passed in rather than imported so this package
	// keeps its stance of depending on nothing but internal/download and
	// internal/version (ollama_backend.go records the same rule for
	// internal/hardware).
	Probe func(ctx context.Context, path string) (bool, string)
	// Install fetches and extracts the pinned release.
	Install func(ctx context.Context) error
}

// NewOllamaConvergeDeps wires the plain case: the bundled installer under
// baseDir, with no progress rendering and no ROCm overlay decision. The
// CLI builds its own deps instead, because it has both.
func NewOllamaConvergeDeps(baseDir string, probe func(ctx context.Context, path string) (bool, string)) OllamaConvergeDeps {
	inst := NewOllamaInstaller(baseDir)
	return OllamaConvergeDeps{
		Present:    inst.Active,
		BinaryPath: inst.BinaryPath,
		Probe:      probe,
		Install:    func(ctx context.Context) error { return inst.Install(ctx, nil) },
	}
}

// ConvergeOllama brings an already-installed bundled engine up to the pin
// and reports what it decided, so the caller can say so whether or not it
// acted.
func ConvergeOllama(ctx context.Context, d OllamaConvergeDeps) (OllamaConvergeDecision, error) {
	facts := OllamaConvergeFacts{Installed: d.Present(), Pin: OllamaPinnedVersion}
	if facts.Installed {
		_, facts.Version = d.Probe(ctx, d.BinaryPath())
	}
	decision := DecideOllamaConverge(facts)
	if !decision.Install {
		return decision, nil
	}
	if err := d.Install(ctx); err != nil {
		return decision, fmt.Errorf("converge bundled engine to %s: %w", OllamaPinnedVersion, err)
	}
	return decision, nil
}
