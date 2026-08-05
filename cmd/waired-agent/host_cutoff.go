// The install-time host cutoff's WIRING half (#496): getting the probe
// model onto the host, taking the measurement, and acting on the verdict.
// The policy is internal/router/host_cutoff.go; the measurement is
// host_cutoff_probe.go.
//
// WHERE THIS RUNS, and why not where #496 said. The issue put the cutoff
// beside the install-time model selection, which is
// maybeSelectBundledModelForFreshInstall — and that runs at main.go:219,
// before the logger exists and long before any engine does. A probe needs
// a running engine and its ~1 GB model, so it cannot run there.
//
// It runs instead at the moment the decision is actually worth taking:
// the daemon has an engine up, has chosen a bundled model for itself
// (nobody else chose one for it), and is about to download 20-45 GB of
// weights. That is the last point before the cost lands and the first
// point where the measurement is possible. The verdict reaches the same
// place SelectInstallModel's ok=false reaches — local inference off with
// the #465 opt-in — which is what
// docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
// decision 6 requires of it.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

const (
	// hostCutoffPullPoll / hostCutoffPullTimeout pace the wait for the
	// probe model's own download. PullModel is asynchronous, and the
	// cutoff has nothing to measure until the weights land. ~1 GB, so the
	// ceiling is about a connection slow enough that the 20-45 GB model
	// behind it was never going to arrive either.
	hostCutoffPullPoll    = 2 * time.Second
	hostCutoffPullTimeout = 20 * time.Minute
)

// hostMeetsRecommendedSpec measures this host and reports whether it
// clears the cutoff. decided is false whenever no verdict could be
// reached — no engine, no probe model, no timing counters, a truncated
// prefill, a machine too busy to answer — and callers must carry on
// unchanged in that case. An unrun measurement is not evidence.
//
// Opinionated about one thing only: it does not run when the operator has
// said something. A pin, a force, or a preferred model is a person
// choosing to serve on this host, and #465's default is not theirs to
// override (waired-ai/waired#1056: refusal is reserved for certain OOM,
// and this refuses nothing — it sets a default).
func (p *agentInferenceProvider) hostMeetsRecommendedSpec(ctx context.Context) (ok, decided bool) {
	if p == nil || p.ollama == nil {
		return false, false
	}
	tag, err := p.hostCutoffProbeTag(ctx)
	if err != nil {
		p.logger.Info("host cutoff: skipping the measurement", "err", err)
		return false, false
	}
	if err := p.ensureHostCutoffProbeModel(ctx, tag); err != nil {
		p.logger.Info("host cutoff: probe model unavailable; skipping the measurement",
			"model", router.HostCutoffProbeModelID, "err", err)
		return false, false
	}

	probe, err := measureHostCutoff(ctx, hostCutoffDeps{
		BaseURL:     p.ollama.BaseURL(),
		EngineModel: tag,
		Logger:      p.logger,
		// Unique per run: a repeat that shares a prefix is answered from
		// the engine's KV cache at a rate no host can achieve.
		Nonce: fmt.Sprintf("hostcutoff-%d", time.Now().UnixNano()),
	})
	if err != nil {
		p.logger.Info("host cutoff: measurement did not complete; leaving local inference as configured",
			"err", err)
		return false, false
	}
	ok, decided = probe.MeetsRecommendedSpec()
	if !decided {
		// Reached when the engine answered but the prefill was not the
		// depth asked for — the silent-truncation case Measured() guards.
		p.logger.Warn("host cutoff: the engine did not prefill the depth asked for; no verdict",
			"prompt_tokens", probe.PromptTokens, "want_tokens", router.HostCutoffProbeDepthTokens)
		return false, false
	}
	p.logger.Info("host cutoff: measured",
		"turn_seconds", fmt.Sprintf("%.1f", probe.TurnSeconds()),
		"budget_seconds", fmt.Sprintf("%.0f", router.HostCutoffTurnBudgetSeconds),
		"prefill_tok_s", fmt.Sprintf("%.0f", probe.PrefillTokps),
		"decode_tok_s", fmt.Sprintf("%.1f", probe.DecodeTokps),
		"prompt_tokens", probe.PromptTokens,
		"meets_recommended_spec", ok)
	return ok, true
}

// hostCutoffProbeTag resolves the probe model to the engine-native tag to
// measure on. It fails rather than substituting another model: the
// threshold is calibrated against this one, and a number measured on
// something else is not comparable to it.
func (p *agentInferenceProvider) hostCutoffProbeTag(ctx context.Context) (string, error) {
	manifest, ok := catalog.LookupByAlias(router.HostCutoffProbeModelID, p.manifests)
	if !ok {
		return "", fmt.Errorf("probe model %s is not in this build's catalog", router.HostCutoffProbeModelID)
	}
	engine := p.servingEngine()
	if engine != catalog.RuntimeOllama {
		// The counters the measurement reads are ollama's. vLLM's
		// OpenAI-compat surface does not expose them, and a vLLM host has
		// a GPU by construction — it is not the host this is looking for.
		return "", fmt.Errorf("serving engine is %s; the cutoff probe reads ollama's counters", engine)
	}
	variant, pullable := router.FirstPullableVariant(manifest, engine, p.engineVersionFor(ctx, engine))
	if !pullable || variant.Source.Tag == "" {
		return "", fmt.Errorf("no %s variant of %s this engine can load", engine, manifest.ModelID)
	}
	return variant.Source.Tag, nil
}

// ensureHostCutoffProbeModel gets the probe model onto the host and waits
// for it, because a measurement cannot start before the weights do.
//
// The ~1 GB it may download is not wasted on the host that fails: that is
// the same model the below-floor opt-in offers (quality_tier 12, the
// smallest entry the catalog offers), so a host told local inference is
// off already has the one model it can actually run sitting on disk.
func (p *agentInferenceProvider) ensureHostCutoffProbeModel(ctx context.Context, tag string) error {
	if p.engineServesTag(ctx, tag) {
		return nil
	}
	if _, err := p.PullModel(ctx, router.HostCutoffProbeModelID); err != nil {
		return fmt.Errorf("pull probe model: %w", err)
	}
	deadline := time.Now().Add(hostCutoffPullTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hostCutoffPullPoll):
		}
		st, _ := p.store.Load()
		switch st.Models[router.HostCutoffProbeModelID].State {
		case catalog.ModelStateReady:
			return nil
		case catalog.ModelStateFailed:
			return fmt.Errorf("probe model download failed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("probe model did not finish downloading within %s", hostCutoffPullTimeout)
		}
	}
}

// hostCutoffIsStillOurs reports whether the local-inference toggle is
// unset — nobody, and nothing, has expressed a choice on this axis yet.
// Only then may the cutoff decide it.
//
// Without this the opt-in does not survive a restart. The cutoff turns
// local inference off by writing exactly the file `waired inference on`
// writes to; an operator opts back in, the daemon restarts, the bundled
// model still is not on disk, the pre-pull path runs again — and the
// cutoff measures the same slow host and takes their choice away. #465
// calls the off state "a default with a working opt-in", and an opt-in
// that is silently reverted on the next boot is neither.
//
// An unreadable state dir reads as "unset", which costs at most one
// re-measure; refusing to measure at all on a read error would be the
// worse failure, since a fresh install's file is legitimately absent.
func (p *agentInferenceProvider) hostCutoffIsStillOurs() bool {
	desired, err := state.ReadDesiredInferenceState(p.stateDir)
	if err != nil {
		p.logger.Warn("host cutoff: could not read the local-inference choice; measuring anyway", "err", err)
		return true
	}
	if desired != "" {
		p.logger.Info("host cutoff: skipped — the local-inference choice was already made here",
			"desired", string(desired))
		return false
	}
	return true
}

// applyHostCutoff is the whole cutoff as the pre-pull path sees it:
// measure, and on a host that does not clear the budget turn local
// inference off and report why. It returns whether the caller should go
// on to download the bundled model.
//
// Turning it OFF is a default, not a refusal: #465's opt-in
// (`waired inference on`, the tray, the browser wizard) works immediately
// afterwards and starts the engine, and the node enrols and relays
// either way. Which is exactly why the message has to name the opt-in.
func (p *agentInferenceProvider) applyHostCutoff(ctx context.Context) bool {
	if !p.hostCutoffIsStillOurs() {
		return true
	}
	ok, decided := p.hostMeetsRecommendedSpec(ctx)
	if !decided || ok {
		return true
	}
	p.logger.Warn("this host is below the recommended spec for local inference: one coding-agent turn "+
		"would take longer than the budget, on the smallest model there is. Local inference starts off "+
		"and this node runs as a gateway/relay — it can still route inference to mesh peers. "+
		"Turn it on anyway with `waired inference on`.",
		"budget_seconds", fmt.Sprintf("%.0f", router.HostCutoffTurnBudgetSeconds))
	if p.disableInference == nil {
		// --disable-inference already took the subsystem out; there is no
		// controller to persist through and nothing to turn off.
		return false
	}
	if err := p.disableInference(); err != nil {
		// The download is still the wrong thing to start. Saying so
		// matters: without the persisted state the next boot will measure
		// again and reach the same answer, which is correct but silent.
		p.logger.Warn("host cutoff: could not persist local inference off; skipping the download anyway", "err", err)
	}
	return false
}
