//go:build !linux

package main

import (
	"context"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// vLLM serving is Linux-only (the upstream wheels ship CUDA Linux builds;
// see internal/runtime/vllm.go and its stub files). These stubs keep
// cmd/waired-agent compiling on Windows/macOS, where engineViable("vllm")
// already returns false so neither is reached in practice.

func (p *agentInferenceProvider) dispatchHFPull(_ context.Context, _ catalog.Manifest, _ catalog.Variant, _ string) error {
	// Wrapped so the setup reconciler reports this as a host limitation
	// rather than "that model is not available" — the model is fine, this
	// OS just cannot serve it (waired-agent#134).
	return fmt.Errorf("vllm serving is only supported on linux: %w", errUnsupportedSource)
}

func (p *agentInferenceProvider) bootstrapVLLM(_ context.Context) {
	p.logger.Error("vllm serving was selected but is not supported on this OS; falling back requires ollama")
}
