package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// maybeSelectBundledModelForFreshInstall runs the install-time, hardware-aware
// bundled-model selection on the daemon boot path — but only on a genuinely
// fresh install with no operator inference preference expressed — and persists
// the verdict to agent.json.
//
// The daemon-mediated `waired init` (waired#756) enrolls via setup.Enroll,
// which — unlike the local init's setup.Init — never runs the ConfigureInference
// hook, so the local path's hardware model sizing / under-spec disable never
// happened. Without this the daemon boots inference-enabled with the fixed
// default model and pulls it in full even on a host too small to serve it.
// Persisting to agent.json makes the choice stable and inspectable, and makes
// this a one-shot: a written agent.json makes every later boot skip it.
//
// Best-effort and non-interactive by construction (the daemon has no TTY): any
// failure keeps the pristine defaults and warns on stderr, never aborting boot.
// It MUST run before the Inference.Enabled gate in run() so an under-spec
// verdict (EnableInference=false) correctly forces the --disable-inference path.
func maybeSelectBundledModelForFreshInstall(cfg *agentconfig.Config, disableInference bool, agentJSONPath, stateDir string, fs *flag.FlagSet) {
	prefPath := filepath.Join(stateDir, "inference", agentconfig.PreferenceFileName)
	if !shouldAutoSelectBundledModel(
		disableInference,
		fileExists(agentJSONPath),
		fileExists(prefPath),
		anyInferenceFlagSet(fs),
		os.Environ(),
	) {
		return
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled catalog unavailable (%v); keeping default model %s\n",
			err, cfg.Inference.BundledModelID)
		return
	}

	// Fresh install with no operator preference ⇒ neither pinned nor forced.
	prof := hardware.NewProfiler("").Profile(context.Background())
	sel, err := setup.SelectBundledModel(setup.BundledModelInputs{
		Hardware:      prof,
		Manifests:     manifests,
		Inference:     cfg.Inference,
		StateDir:      stateDir,
		FloorTier:     router.InstallQualityFloorTier,
		FreeDiskBytes: hardware.FreeDiskBytes,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: bundled model selection failed (%v); keeping %s\n",
			err, cfg.Inference.BundledModelID)
		return
	}

	applyBundledSelection(cfg, sel)
	for _, n := range sel.Notes {
		fmt.Fprintf(os.Stderr, "waired-agent: %s\n", n)
	}
	if !sel.EnableInference {
		fmt.Fprintf(os.Stderr,
			"waired-agent: host under-spec for local inference — running as gateway/relay. "+
				"Re-run `waired init` interactively (or pass --inference-enabled=true) to override.\n")
	}

	if err := cfg.Save(agentJSONPath); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not persist inference selection to %s (%v)\n", agentJSONPath, err)
	}
}

// applyBundledSelection folds SelectBundledModel's verdict into cfg.Inference:
// the chosen model id, whether local inference runs at all (under-spec ⇒ off),
// and — when disk is short — turning off the startup pull. Pure; the caller
// persists cfg afterward.
func applyBundledSelection(cfg *agentconfig.Config, sel setup.BundledModelSelection) {
	cfg.Inference.BundledModelID = sel.ModelID
	cfg.Inference.Enabled = sel.EnableInference
	if sel.SkipPull {
		cfg.Inference.PullOnStartup = false
	}
}

// shouldAutoSelectBundledModel reports whether the daemon should run the
// install-time hardware-aware bundled-model selection. It fires only on a
// pristine fresh install: no persisted agent.json, no persisted model
// preference, and no operator inference preference expressed via a
// --disable-inference / --inference-* flag or a WAIRED_INFERENCE_* env var. Any
// such signal means the operator has already chosen, so the daemon defers to
// them. Pure (every probe is passed in) so the decision is table-testable.
func shouldAutoSelectBundledModel(disableInference, agentJSONExists, preferenceExists, inferenceFlagSet bool, environ []string) bool {
	if disableInference || agentJSONExists || preferenceExists || inferenceFlagSet {
		return false
	}
	for _, e := range environ {
		if strings.HasPrefix(e, "WAIRED_INFERENCE_") {
			return false
		}
	}
	return true
}

// anyInferenceFlagSet reports whether any inference-controlling flag was
// explicitly set on the command line (--disable-inference or any --inference-*).
func anyInferenceFlagSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "disable-inference" || strings.HasPrefix(f.Name, "inference-") {
			set = true
		}
	})
	return set
}

// fileExists reports whether path resolves to an existing filesystem entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
