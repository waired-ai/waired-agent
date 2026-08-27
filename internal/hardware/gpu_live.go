package hardware

import "context"

// LiveGPUMemory is one live per-device memory reading.
//
// FreeMB is 0 when the vendor's tool reported no free figure at all;
// callers must read that as "no evidence", never as "the card is full"
// (see TightestGPUFreeMB).
type LiveGPUMemory struct {
	Vendor  string
	Model   string
	UUID    string
	TotalMB int
	FreeMB  int
}

// LiveGPUFree re-runs the vendor detectors and returns each device's
// CURRENT free memory.
//
// Deliberately not Profiler.Profile(). That path freezes the free
// reading at the first sample (Profiler.freezeVRAMFree, ratified in
// docs/decisions/20260813/1120 §Decision 5) because it feeds the sizing
// BUDGET, and a budget that shrinks by our own resident weights spirals.
// This asks the opposite question — how much of the card is left AFTER
// our own load — and its answer is only ever read as evidence about a
// load that has already happened, never as a budget for the next one.
// Keeping them in two functions is what keeps that distinction
// enforceable.
//
// Detection errors are dropped: a partial answer about which cards could
// be read is exactly what the caller wants, and a device that could not
// be enumerated is covered by ok=false below.
//
// One reading is not like the others: on a unified-memory AMD host
// rocm-smi reports the whole BIOS carve-out as the device total
// (cmd/waired/peers_test.go pins that for Strix Halo), so a free figure
// derived from it is not the quantity a discrete card's free figure is.
// The only consumer today gates itself on !UnifiedMemory before asking
// (cmd/waired-agent/inference_ollama_verify.go), and a future one that
// does not must make that decision deliberately.
func LiveGPUFree(ctx context.Context) []LiveGPUMemory {
	gpus, _, _ := composeDetectors(ctx, vendorDetectors)
	out := make([]LiveGPUMemory, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, LiveGPUMemory{
			Vendor:  g.Vendor,
			Model:   g.Model,
			UUID:    g.UUID,
			TotalMB: g.VRAMTotalMB,
			FreeMB:  g.VRAMFreeMB,
		})
	}
	return out
}

// TightestGPUFreeMB reduces LiveGPUFree to the one figure a post-load
// fit check needs: the SMALLEST free reading across the devices that
// reported one.
//
// Smallest, because ollama spreads layers over the whole pool and
// /api/ps does not say which device holds what, so the binding
// constraint is the tightest card.
//
// ok is false when no device reported a free figure: an AMD host with
// no rocm-smi on PATH (the Windows registry fallback carries capacity
// only — see gpu_amd_windows.go), an Apple host (the UMA budget comes
// from Profile.UsableVRAMMB, not from a per-device reading), an Intel
// host (no detector exists), and any NVIDIA driver that rejected
// memory.free and fell back to the basic query.
//
// It is NOT the whole AMD fleet any more. That was true until
// waired-agent#1056: the parser asked rocm-smi for used memory and
// dropped the column.
//
// Callers MUST treat ok=false as "no evidence" and keep whatever
// behaviour they had without this reading; treating it as 0 would
// degrade every such host on a measurement nobody took.
func TightestGPUFreeMB(ctx context.Context) (mb int, ok bool) {
	for _, g := range LiveGPUFree(ctx) {
		if g.FreeMB <= 0 {
			continue
		}
		if !ok || g.FreeMB < mb {
			mb, ok = g.FreeMB, true
		}
	}
	return mb, ok
}
