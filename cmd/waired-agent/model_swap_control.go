package main

import (
	"context"
	"log/slog"
)

// modelSwapController backs the management CatalogConfig.ApplyModelSwitch seam:
// it applies an operator's preferred-model switch in process (#812) so the
// whole agent no longer restarts to change models. Like engineController it
// holds the daemon's long-lived agentCtx and runs the switch against it — the
// per-request HTTP context is cancelled the moment the handler returns, which
// would abort the engine bounce mid-flight.
type modelSwapController struct {
	provider *agentInferenceProvider
	// agentCtx is the daemon's long-lived context. The switch (and the
	// pull it may dispatch) must outlive the request, so it never uses the
	// handler's context.
	agentCtx context.Context
	logger   *slog.Logger
}

func newModelSwapController(ctx context.Context, provider *agentInferenceProvider, logger *slog.Logger) *modelSwapController {
	return &modelSwapController{provider: provider, agentCtx: ctx, logger: logger}
}

// ApplyModelSwitch applies the in-process switch and reports whether a
// background pull was started. The request ctx is deliberately ignored (see the
// type doc); the switch runs on agentCtx. A non-nil error (a cross-engine
// target signalled by errSwapNeedsRestart, or a validation failure) tells the
// handler to fall back to the supervised restart.
func (c *modelSwapController) ApplyModelSwitch(_ context.Context, modelID string) (bool, error) {
	return c.provider.SwapPreferredModel(c.agentCtx, modelID)
}

// ApplyNoModelSelected applies the operator's "don't download a model
// now" choice in process (waired-agent#586) — the management handler has
// already persisted it.
func (c *modelSwapController) ApplyNoModelSelected() {
	c.provider.applyNoModelSelected()
}

// NoteModelChoicePending registers or withdraws `waired init`'s claim
// that the model question is about to be asked at the terminal (#586).
func (c *modelSwapController) NoteModelChoicePending(pending bool) {
	c.provider.noteModelChoicePending(pending)
}
