package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// maybeSelectBundledModelForFreshInstall runs the install-time, hardware-aware
// bundled-model selection on the daemon boot path — but only on a genuinely
// fresh install with no operator inference preference expressed — and persists
// the verdict to agent.json.
//
// The daemon-mediated `waired init` (waired#756) enrolls via setup.Enroll,
// which — unlike the local init's setup.Init — never runs the
// ConfigureInference hook, so the local path's hardware model sizing and its
// below-recommended-spec default never happened. Without this the daemon boots
// inference-enabled with the fixed default model and pulls it in full even on
// a host too small to serve it. Persisting to agent.json makes the choice
// stable and inspectable, and makes this a one-shot: a written agent.json
// makes every later boot skip it.
//
// Best-effort and non-interactive by construction (the daemon has no TTY): any
// failure keeps the pristine defaults and warns on stderr, never aborting boot.
// It MUST run before planInitialInference in run(), which reads the
// Inference.Enabled this writes as the install-time default for the local
// inference toggle.
func maybeSelectBundledModelForFreshInstall(cfg *agentconfig.Config, disableInference bool, agentJSONPath, stateDir string, fs *flag.FlagSet) {
	prefPath := filepath.Join(stateDir, "inference", agentconfig.PreferenceFileName)
	intent := resolveInferenceIntent(disableInference, fs, os.Environ())
	if !shouldAutoSelectBundledModel(fileExists(agentJSONPath), fileExists(prefPath), intent) {
		return
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled catalog unavailable (%v); %s\n",
			err, describeUnselectedModel(cfg.Inference.BundledModelID))
		return
	}

	// The install-time memory measurement was taken just before this
	// runs (ensureHostMemoryMeasured in run(), pre-engine), so the
	// fresh-install selection already sees the measured OS deduction
	// (#568).
	prof := hardware.NewProfiler("",
		hardware.WithRAMAvailableAtInstall(hostMemoryGB(stateDir, os.Getenv))).
		Profile(context.Background())
	sel, err := setup.SelectBundledModel(setup.BundledModelInputs{
		Hardware:      prof,
		Manifests:     manifests,
		Inference:     cfg.Inference,
		StateDir:      stateDir,
		FreeDiskBytes: hardware.FreeDiskBytes,
		Pinned:        intent.Pinned,
		Forced:        intent.Forced,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled model selection failed (%v); %s\n",
			err, describeUnselectedModel(cfg.Inference.BundledModelID))
		return
	}

	applyBundledSelection(cfg, sel)
	for _, n := range sel.Notes {
		fmt.Fprintf(os.Stderr, "waired-agent: %s\n", n)
	}
	if !sel.EnableInference {
		fmt.Fprintf(os.Stderr,
			"waired-agent: this host is below the recommended spec for local inference, "+
				"so it starts with local inference off and runs as a gateway/relay. "+
				"Turn it on with `waired inference on`.\n")
	}

	if err := cfg.Save(agentJSONPath); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not persist inference selection to %s (%v)\n", agentJSONPath, err)
	}
}

// applyBundledSelection folds SelectBundledModel's verdict into cfg.Inference:
// the chosen model id, whether local inference runs at all (below the recommended spec ⇒ off),
// and — when disk is short — turning off the startup pull. Pure; the caller
// persists cfg afterward.
func applyBundledSelection(cfg *agentconfig.Config, sel setup.BundledModelSelection) {
	cfg.Inference.BundledModelID = sel.ModelID
	cfg.Inference.Enabled = sel.EnableInference
	if sel.SkipPull {
		cfg.Inference.PullOnStartup = false
	}
}

// inferenceIntent is what the operator said about the MODEL, as opposed to
// how the engine is wired. Ports, cache sizes, TTFB budgets and vLLM knobs
// configure an engine; they cannot say which model belongs on a machine, and
// treating them as if they could is how a fresh install ends up pre-pulling a
// model its host cannot serve.
//
// Skip dominates: when it is set the other two are moot.
type inferenceIntent struct {
	// Skip means auto-selection must not run at all — inference is off,
	// or the operator's own preferred model owns the serving path (#306).
	Skip bool
	// Pinned means a bundled model id was named, so selection honours it
	// verbatim instead of ranking the catalog.
	Pinned bool
	// Forced means inference was explicitly turned on, so an under-spec
	// host is warned rather than silently disabled.
	Forced bool
	// Enablement is what the operator said about whether local inference
	// runs at all, or nil when they said nothing. Skip cannot answer
	// that: it also covers --disable-inference (a transient kill switch)
	// and an operator's own preferred model, neither of which is a
	// durable statement about this axis. planInitialInference reads this.
	Enablement *bool
}

// resolveInferenceIntent reads the operator's model-level intent off the boot
// flags and environment. Pure (every input is passed in) so the decision is
// table-testable, per the (facts) -> plan seam style.
//
// Env is read first and flags second because that is the config precedence
// the rest of the daemon uses (defaults < agent.json < env < flags), so a
// flag must win over an env var on the same axis.
func resolveInferenceIntent(disableInference bool, fs *flag.FlagSet, environ []string) inferenceIntent {
	var (
		in inferenceIntent
		// nil until the operator says something about whether inference
		// runs at all; that is a different state from "said off".
		enabled *bool
	)

	for _, e := range environ {
		key, val, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		switch key {
		case "WAIRED_INFERENCE_ENABLED":
			enabled = parseEnablement(val)
		case "WAIRED_INFERENCE_BUNDLED_MODEL_ID":
			in.Pinned = true
		case "WAIRED_INFERENCE_PREFERRED_MODEL_ID":
			in.Skip = true
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "inference-enabled":
			enabled = parseEnablement(f.Value.String())
		case "inference-bundled-model-id":
			in.Pinned = true
		case "inference-preferred-model-id":
			in.Skip = true
		}
	})

	// --disable-inference is the operator's transient kill switch, so it
	// outranks a contradictory enablement for THIS boot. It deliberately
	// does not reach in.Enablement: a flag that is not persisted anywhere
	// must not be recorded as a durable choice.
	in.Enablement = enabled
	in.Skip = in.Skip || disableInference || (enabled != nil && !*enabled)
	in.Forced = !in.Skip && enabled != nil && *enabled
	return in
}

// parseEnablement reads an enablement value into "on" / "off". An unreadable
// value resolves to off: the operator did say something about whether
// inference runs, and deferring is safer than guessing they meant on —
// agentconfig.MergeEnv rejects the same value a moment later anyway.
func parseEnablement(val string) *bool {
	on, err := strconv.ParseBool(val)
	if err != nil {
		on = false
	}
	return &on
}

// shouldAutoSelectBundledModel reports whether the daemon should run the
// install-time hardware-aware bundled-model selection. It fires only on a
// pristine fresh install — no persisted agent.json, no persisted model
// preference — that the operator has not opted out of.
//
// A pin or a force does NOT stop selection: those are inputs to it
// (BundledModelInputs.Pinned / .Forced), and skipping the selector would
// throw away the hardware profile the operator never asked to discard.
// Pure (every probe is passed in) so the decision is table-testable.
func shouldAutoSelectBundledModel(agentJSONExists, preferenceExists bool, intent inferenceIntent) bool {
	return !agentJSONExists && !preferenceExists && !intent.Skip
}

// describeUnselectedModel is the tail of the two "selection did not happen"
// warnings. There is no compiled-in default any more, so on a fresh install
// the id is empty and the honest report is that nothing was chosen — not
// that some named model is being "kept".
func describeUnselectedModel(modelID string) string {
	if modelID == "" {
		return "no bundled model selected; local inference starts without one " +
			"(choose one with `waired models pull`)"
	}
	return fmt.Sprintf("keeping the configured model %s", modelID)
}

// fileExists reports whether path resolves to an existing filesystem entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
