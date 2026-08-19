package runtime

// Converging the vLLM venv onto the pin set (#843).
//
// vLLM had a pin and no converge, so a host that ran `waired init` kept
// whatever release it was set up with for ever, while every new agent
// build claimed to serve with VLLMPinnedVersion. Ollama got this in
// #826; the reasoning is the same one sentence — converge an engine that
// is already there, never create one that is not — but the two engines
// are not symmetric, and three differences shape what is below.
//
//  1. A stale vLLM is not "old but working". The product's vLLM-facing
//     tables are read out of the pinned release's own registry: the
//     tool-parser names in cmd/waired-agent/inference_vllm_toolparser.go
//     and the serve flags in VLLMAdapter.commandArgs. vLLM rejects an
//     unregistered --tool-call-parser at start-up and exits 2 on an
//     unknown flag, so on a host whose venv predates either, the engine
//     does not start at all and EnsureRunning's retries all fail. That is
//     the same shape as Ollama's exact-match pin, arrived at differently,
//     and it is why the rule here is also exact match rather than "at
//     least the pin".
//
//  2. The converge never removes what is there. A version move builds a
//     NEW <baseDir>/<version>/.venv and swaps the `current` symlink at
//     the end, so the environment in use is untouched and a failure
//     leaves it exactly as found; a companion-pin move re-resolves the
//     wheels into the environment already there. Neither path clears an
//     environment — that is `waired runtimes install vllm`'s job, and
//     the difference is InstallOpts.Recreate.
//
//  3. It costs disk twice. Because the new venv is a new directory, a
//     host needs room for both, and nothing removed the old one — only
//     `waired runtimes uninstall` did. Hence the free-space fact below
//     and PruneOtherVersions afterwards.
//
// The pin SET, not the vLLM version alone, is what a venv is compared
// against. The version directory is named after the vLLM release, so a
// host whose hf_transfer / transformers / interpreter pin moved on its
// own looks up to date by name; the recorded set is what makes that
// visible. Reconciling one of those is also cheap — the environment is
// already there, so the venv stage is skipped and pip resolves the small
// wheel against uv's cache instead of fetching torch again. The one
// member it cannot reconcile that way is the interpreter, which is why
// DecideVLLMConverge reports that difference as blocked rather than
// acting on it.

import (
	"context"
	"fmt"
)

// VLLMConvergeFreeBytes is the free space a converge wants before it
// starts. The venv the CLI quotes as "~6 GB" plus room for uv's cache
// and the interpreter tree, because the OLD venv is still on disk while
// the new one builds — pruning only happens after the swap.
//
// A pre-flight rather than a mid-install ENOSPC: the failure it prevents
// is an unattended background install filling the root filesystem of a
// host that is serving from it.
const VLLMConvergeFreeBytes = 8 << 30

// VLLMConvergeFacts is what the decision is made from. Facts, not
// handles, for the same reason as OllamaConvergeFacts: three callers act
// on this policy — `waired runtimes upgrade vllm`, the daemon's start-up
// backstop, and the installer scripts through the first — and none of
// them may grow its own variant of the rule.
type VLLMConvergeFacts struct {
	// Installed reports whether a usable venv is active here.
	Installed bool

	// Version is the vLLM release that venv holds, from the `current`
	// symlink's target directory. Unlike Ollama's this costs no
	// subprocess: the installer names the directory after the version it
	// built, so reading the link IS reading the version.
	Version string

	// Recorded is the pin set that venv was built from, and HasRecord
	// says whether there was one to read. An install made before the
	// record existed has none, and that is NOT drift — see the decision.
	Recorded  VLLMPinSet
	HasRecord bool

	// Want is this build's set, normally WantedVLLMPins().
	Want VLLMPinSet

	// FreeBytes is the space available where the venv lives, or 0 when
	// it could not be read. Unknown does not block: an unreadable statfs
	// is not evidence of a full disk, and the install has its own ENOSPC.
	FreeBytes int64
}

// VLLMConvergeDecision is the answer with the reason it was reached.
//
// Three outcomes, not two. "Nothing to do", "cannot do it right now",
// and "doing it" are printed at the end of an update and logged by the
// daemon, and a host that is short of disk must not read the same as a
// host that is already at the pin.
type VLLMConvergeDecision struct {
	// Install is true when the venv should be (re)built.
	Install bool

	// Blocked is true when a converge is NEEDED but a precondition is
	// not met. Install is false in that case: callers warn rather than
	// act, and the next update tries again.
	Blocked bool

	// Reason is why, in a sentence a person reads in `waired update`
	// output or the agent log.
	Reason string

	// Pruned and PruneErr are filled by ConvergeVLLM after a successful
	// install, never by DecideVLLMConverge. Reclaiming the superseded
	// venv is a separate outcome from converging: failing to remove
	// ~6 GB must not make a converged host report failure.
	Pruned   []string
	PruneErr error
}

// DecideVLLMConverge is the whole policy.
func DecideVLLMConverge(f VLLMConvergeFacts) VLLMConvergeDecision {
	if !f.Installed {
		// #138: `waired init` is the only thing that decides this
		// computer runs models, and a vLLM venv is a ~6 GB answer to
		// that question. An update must not answer it.
		return VLLMConvergeDecision{
			Reason: "no vLLM venv on this host; `waired init` installs one when local inference is turned on",
		}
	}
	need, interpreter := f.drift()
	if need == "" {
		return VLLMConvergeDecision{Reason: f.settled()}
	}
	if interpreter {
		// The one difference a converge cannot close. It reconciles the
		// wheels INTO the environment that is there, which is what keeps
		// it from removing the one the host may be serving from — and an
		// interpreter is not a wheel. Running the install anyway would
		// change nothing and then record a pin set the venv does not
		// have, so the honest answer is to say so and name the verb that
		// can (#843).
		return VLLMConvergeDecision{Blocked: true, Reason: need +
			"; that needs a new virtual environment, which an update will not build over the one in use — " +
			"run `waired runtimes install vllm` when this computer is free"}
	}
	if f.FreeBytes > 0 && f.FreeBytes < VLLMConvergeFreeBytes {
		return VLLMConvergeDecision{Blocked: true, Reason: fmt.Sprintf(
			"%s, but only %.1f GB is free and the rebuild needs about %.0f GB "+
				"(the venv in use stays in place until the new one is ready)",
			need, float64(f.FreeBytes)/(1<<30), float64(VLLMConvergeFreeBytes)/(1<<30))}
	}
	return VLLMConvergeDecision{Install: true, Reason: need}
}

// drift names the first difference from this build's pin set, or "" when
// there is none to act on.
//
// Comparison is exact string equality, and deliberately not through
// internal/version the way Ollama's converge compares. That parser
// exists because `ollama --version` prints prose a human wrote; here
// both sides are strings this product wrote — the directory name is the
// version the installer was ASKED for, and the record holds the
// constants it installed. There is no spelling freedom to absorb, and
// absorbing some would be a hazard rather than a kindness: PyPI post
// releases (0.24.0.post1) differ from their base in exactly the way a
// dotted-core comparison discards, and a pin moved to one is usually a
// pin moved to fix something.
// The second return says the difference is the INTERPRETER, which is the
// one thing an in-place reconcile cannot change.
func (f VLLMConvergeFacts) drift() (string, bool) {
	switch {
	case f.Version == "":
		// Defensive: the version comes from a directory name, so an
		// empty one means the venv cannot describe itself. Same answer
		// as Ollama's unreadable engine — rebuild, because nothing else
		// can be concluded.
		return fmt.Sprintf("the vLLM venv does not name a version; rebuilding at the pin %s", f.Want.VLLM), false
	case f.Version != f.Want.VLLM:
		// Deliberately not "older than". A pin can move backwards when a
		// release is withdrawn, and this build's parser table and serve
		// flags were read out of the pinned release, so a newer venv is
		// as untested as an older one.
		return fmt.Sprintf("vLLM venv is %s, pin is %s", f.Version, f.Want.VLLM), false
	case !f.HasRecord:
		// An install that predates the record (#843), or one whose
		// record cannot be read. Its vLLM version matches and the
		// companion pins cannot be established, so there is no evidence
		// of drift — and rebuilding ~6 GB on the absence of a file would
		// charge every host that installed before this shipped.
		return "", false
	case f.Recorded.HFTransfer != f.Want.HFTransfer:
		return fmt.Sprintf("hf_transfer is %s, pin is %s", f.Recorded.HFTransfer, f.Want.HFTransfer), false
	case f.Recorded.Transformers != f.Want.Transformers:
		return fmt.Sprintf("transformers constraint is %q, pin is %q", f.Recorded.Transformers, f.Want.Transformers), false
	case f.Recorded.Python != f.Want.Python:
		return fmt.Sprintf("venv interpreter is Python %s, pin is %s", f.Recorded.Python, f.Want.Python), true
	default:
		return "", false
	}
}

// settled says WHY there is nothing to do, which is not one answer.
// "Everything matches" and "the version matches and the rest is
// unknowable" are different states, and the second is the one a person
// debugging a companion-pin bump needs to see.
func (f VLLMConvergeFacts) settled() string {
	if !f.HasRecord {
		return fmt.Sprintf(
			"vLLM venv is at the pin %s; it predates the pin record, so the wheels installed with it are not known",
			f.Want.VLLM)
	}
	return "vLLM venv is already at the pin set for " + f.Want.VLLM
}

// VLLMConvergeDeps are the seams. Exported because the two real callers
// install differently — the CLI renders staged progress and hands the
// state dir back to the service user, the daemon just builds — while the
// ORCHESTRATION below must not be written twice.
type VLLMConvergeDeps struct {
	// Active reports the active venv's vLLM version, and whether there
	// is one. One call for both, so presence and version cannot disagree
	// (the mistake #238 fixed for Ollama).
	Active func() (string, bool)
	// Pins reads the recorded pin set for that venv.
	Pins func() (VLLMPinSet, bool)
	// FreeBytes reports free space where the venv lives; 0 for unknown.
	// A seam because reading it is hardware.FreeDiskBytes' job, and this
	// package depends on nothing but internal/download and
	// internal/version (ollama_backend.go records the same rule).
	FreeBytes func() int64
	// Install builds the venv at the pinned set and activates it.
	Install func(ctx context.Context) error
	// Prune removes the superseded venvs, after a successful install.
	Prune func() ([]string, error)
}

// NewVLLMConvergeDeps wires the plain case: the installer under baseDir,
// with no progress rendering and no ownership handoff. The CLI builds
// its own deps instead, because it has both.
func NewVLLMConvergeDeps(baseDir string, freeBytes func() int64) VLLMConvergeDeps {
	inst := NewVLLMInstallerAt(baseDir)
	return VLLMConvergeDeps{
		Active: func() (string, bool) {
			res, ok := inst.Active()
			return res.Version, ok
		},
		Pins:      inst.ActivePins,
		FreeBytes: freeBytes,
		Install: func(ctx context.Context) error {
			_, err := inst.Install(ctx, InstallOpts{}, nil)
			return err
		},
		Prune: inst.PruneOtherVersions,
	}
}

// ConvergeVLLM brings an already-installed venv onto this build's pin
// set and reports what it decided, so the caller can say so whether or
// not it acted.
func ConvergeVLLM(ctx context.Context, d VLLMConvergeDeps) (VLLMConvergeDecision, error) {
	facts := VLLMConvergeFacts{Want: WantedVLLMPins()}
	facts.Version, facts.Installed = d.Active()
	if facts.Installed {
		facts.Recorded, facts.HasRecord = d.Pins()
		if d.FreeBytes != nil {
			facts.FreeBytes = d.FreeBytes()
		}
	}
	decision := DecideVLLMConverge(facts)
	if !decision.Install {
		return decision, nil
	}
	if err := d.Install(ctx); err != nil {
		return decision, fmt.Errorf("converge vLLM to %s: %w", facts.Want.VLLM, err)
	}
	// Only after the swap. Pruning before it would remove the venv the
	// host is still serving from if the install then failed.
	if d.Prune != nil {
		decision.Pruned, decision.PruneErr = d.Prune()
	}
	return decision, nil
}
