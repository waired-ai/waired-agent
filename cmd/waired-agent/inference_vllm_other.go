//go:build !linux

package main

import (
	"context"
	"errors"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// vLLM serving is Linux-only (the upstream wheels ship CUDA Linux builds;
// see internal/runtime/vllm.go and its stub files). These stubs keep
// cmd/waired-agent compiling on Windows/macOS, where engineViable("vllm")
// already returns false so neither is reached in practice.

func (p *agentInferenceProvider) dispatchHFPull(_ context.Context, _ catalog.Manifest, _ catalog.Variant, _ string) error {
	return errors.New("vllm serving is only supported on linux")
}

func (p *agentInferenceProvider) bootstrapVLLM(_ context.Context) {
	p.logger.Error("vllm serving was selected but is not supported on this OS; falling back requires ollama")
}
