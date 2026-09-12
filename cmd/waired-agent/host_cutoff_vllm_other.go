//go:build !linux

package main

import (
	"context"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// measureHostCutoffVLLM is the non-Linux stub. vLLM serving exists only on
// Linux (router.VLLMSupportedOS), so this host can never reach the arm
// that calls it; it refuses rather than pretending, for the same reason
// inference_vllm_other.go does.
func (p *agentInferenceProvider) measureHostCutoffVLLM(context.Context, catalog.Variant, func()) (hostCutoffMeasurement, error) {
	return hostCutoffMeasurement{}, fmt.Errorf("vllm serving is linux-only")
}
