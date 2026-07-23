package main

import (
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// hardwareSummaryFor translates the boot hardware profile into the
// subset broadcast on every InferenceState push. Returns nil when there
// is nothing worth saying (no GPU and no RAM figure), so a host that
// cannot profile itself keeps the field off the wire entirely rather
// than publishing a zero-valued object.
//
// Beyond the peer-display fields (model / VRAM / compute cap) the
// summary carries the three host-fit facts the control plane needs to
// decide which serving engines and catalog models a device may be
// offered during onboarding:
//
//   - UnifiedMemory + UsableVRAMMB reproduce Profile.EffectiveVRAMMB().
//     On Apple Silicon and Strix Halo the raw VRAMTotalMB overstates
//     what the GPU can actually wire down, so comparing a variant's
//     min_vram_mb against it would offer models the host cannot serve.
//   - Vendor, because which engine a host can run is vendor-dependent
//     (vLLM is an NVIDIA path; AMD is served through Ollama's
//     ROCm/Vulkan backends, waired#290) and GPUSummary.Model is
//     documented as free-form and not to be parsed for such decisions.
//
// All three are omitempty, so a non-UMA host with an undetected vendor
// still serializes byte-identically to the pre-addition wire.
func hardwareSummaryFor(prof hardware.Profile) *signer.HardwareSummary {
	gpus := prof.GPUSummary()
	if len(gpus) == 0 && prof.RAMTotalGB <= 0 {
		return nil
	}
	summary := &signer.HardwareSummary{
		RAMTotalGB:    prof.RAMTotalGB,
		UnifiedMemory: prof.UnifiedMemory,
		UsableVRAMMB:  prof.UsableVRAMMB,
	}
	for _, g := range gpus {
		summary.GPUs = append(summary.GPUs, signer.HardwareGPUSummary{
			Model:       g.Model,
			VRAMTotalMB: g.VRAMTotalMB,
			ComputeCap:  g.ComputeCap,
			Vendor:      g.Vendor,
		})
	}
	return summary
}
