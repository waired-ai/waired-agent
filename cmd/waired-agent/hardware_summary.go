package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// hardwareResampleInterval is how long a published host profile may go
// without being re-detected (#387).
//
// The summary used to be sampled once at boot and captured as a pointer,
// so a host that gained a GPU or installed a driver kept reporting the
// old answer until the daemon restarted — and the control plane's
// onboarding host-fit kept scoring against it. Re-detection is paced by
// the Profiler's own TTL rather than a ticker of ours, so the probe loop
// can read the getter every tick and pay a real probe only this often.
//
// Five minutes is well inside the existing envelope: the management API
// drives the same detectors through a 30 s-TTL profiler on ordinary
// request paths, so an open admin page already re-detects ten times more
// often than this.
const hardwareResampleInterval = 5 * time.Minute

// hardwareSummaryFn returns the getter the inference probe loop reads to
// decorate each push, or nil when there is no profiler to read (which is
// how --disable-inference stays completely silent — see
// runHardwareOnlyReport).
//
// A getter rather than a value: what a host IS can change while the
// daemon runs, and the pointer this replaced could only ever carry what
// was true at boot.
func hardwareSummaryFn(ctx context.Context, p *hardware.Profiler) func() *signer.HardwareSummary {
	if p == nil {
		return nil
	}
	return func() *signer.HardwareSummary { return hardwareSummaryFor(p.Profile(ctx)) }
}

// hardwareSummaryFor translates a hardware profile into the subset
// broadcast on the InferenceState push. Returns nil when there is
// nothing worth saying (no GPU and no RAM figure), so a host that
// cannot profile itself keeps the field off the wire entirely rather
// than publishing a zero-valued object.
//
// Since #387 this is published by an engine-LESS host too, not only as a
// rider on a successful engine probe: the browser setup wizard scores
// its catalog against what the control plane knows about the machine,
// and the window in which it runs is precisely the window in which there
// is no engine to probe.
//
// Beyond the peer-display fields (model / VRAM / compute cap) the
// summary carries the host-fit facts the control plane needs to decide
// which serving engines and catalog models a device may be offered
// during onboarding:
//
//   - UnifiedMemory + UsableVRAMMB reproduce Profile.EffectiveVRAMMB().
//     On Apple Silicon and Strix Halo the raw VRAMTotalMB overstates
//     what the GPU can actually wire down, so comparing a variant's
//     min_vram_mb against it would offer models the host cannot serve.
//   - Vendor, because which engine a host can run is vendor-dependent
//     (vLLM is an NVIDIA path; AMD is served through Ollama's
//     ROCm/Vulkan backends, waired#290) and GPUSummary.Model is
//     documented as free-form and not to be parsed for such decisions.
//   - MemoryBandwidthSpecGBs, the unified pool's published peak. It is
//     what lets the fit rules REFUSE a model for being too slow rather
//     than merely annotate it: a peak is an upper bound, so "too slow
//     even at peak" is a claim about this host (#251). Publishing the
//     number rather than the chip name is also what keeps the "do not
//     parse Model" rule intact for consumers.
//   - CarveOutVRAMMB, so the capacity gate can add RAM and VRAM without
//     counting one physical pool twice (hostfit.TotalMemoryMB). It is
//     the quantity, not a flag, because the question it answers —
//     "was that VRAM figure read, or derived from RAM" — has to be
//     answered by the side that produced the figure.
//
// All are omitempty, so a non-UMA host with an undetected vendor still
// serializes byte-identically to the pre-addition wire — as does a
// unified host whose part is not yet in the chip table.
func hardwareSummaryFor(prof hardware.Profile) *signer.HardwareSummary {
	gpus := prof.GPUSummary()
	if len(gpus) == 0 && prof.RAMTotalGB <= 0 {
		return nil
	}
	summary := &signer.HardwareSummary{
		RAMTotalGB: prof.RAMTotalGB,
		// The persisted install-time figure, never the live
		// RAMAvailableGB — fixed for the life of the install, so it
		// adds no map churn and never counts a resident model against
		// the host serving it (#568).
		RAMAvailableGB: prof.RAMAvailableAtInstallGB,
		// ...and when it was taken (#699). A consumer cannot tell the
		// figure above from a live reading without it, and the figure is
		// emphatically not live.
		RAMAvailableMeasuredAt: prof.RAMAvailableAtInstallMeasuredAt,
		UnifiedMemory:          prof.UnifiedMemory,
		UsableVRAMMB:           prof.UsableVRAMMB,
		MemoryBandwidthSpecGBs: prof.MemoryBandwidthSpecGBs,
		CarveOutVRAMMB:         prof.CarveOutVRAMMB,
	}
	for _, g := range gpus {
		summary.GPUs = append(summary.GPUs, signer.HardwareGPUSummary{
			Model:       g.Model,
			VRAMTotalMB: g.VRAMTotalMB,
			VRAMFreeMB:  g.VRAMFreeMB,
			ComputeCap:  g.ComputeCap,
			Vendor:      g.Vendor,
		})
	}
	return summary
}
