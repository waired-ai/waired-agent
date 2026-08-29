package router

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The serve-flag derivations moved here from cmd/waired-agent so the GPU e2e
// lane can call them (waired-agent#955). These tables came with them: they
// are the cheap half of the coverage, and the lane is the half that proves
// the derived values actually reach the engine.

// Prefill chunking and KV offloading (waired-agent#887).

func TestVLLMMaxNumBatchedTokens(t *testing.T) {
	small := hardware.Profile{GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}}}
	big := hardware.Profile{GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 81920}}}
	mixed := hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "nvidia", VRAMTotalMB: 81920},
		{Vendor: "nvidia", VRAMTotalMB: 24467},
	}}
	none := hardware.Profile{}

	for _, tc := range []struct {
		name        string
		maxModelLen int
		hw          hardware.Profile
		override    int
		want        int
	}{
		{"under 70 GiB raises upstream's 2048 to 4096", 200704, small, 0, 4096},
		{"at or above 70 GiB keeps upstream's own 8192", 200704, big, 0, 8192},
		// A flat 4096 on the big card would LOWER the chunk below what
		// vLLM picks for itself — a regression from a perf change.
		{"the smallest card decides, not the first", 200704, mixed, 0, 4096},
		{"no NVIDIA card visible falls to the small value", 200704, none, 0, 4096},
		{"an override wins outright", 200704, small, 16384, 16384},
		{"clamped to a window smaller than the chunk", 2048, small, 0, 2048},
		{"an unknown window does not clamp", 0, small, 0, 4096},
		// vLLM raises a ValueError when the chunk is below max_num_seqs.
		{"never below vLLM's max_num_seqs default", 64, small, 0, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VLLMMaxNumBatchedTokens(tc.maxModelLen, tc.hw, tc.override); got != tc.want {
				t.Errorf("VLLMMaxNumBatchedTokens(%d, ..., %d) = %d, want %d",
					tc.maxModelLen, tc.override, got, tc.want)
			}
		})
	}
}

func TestVLLMKVOffloadingGiB(t *testing.T) {
	standing := hardware.Profile{RAMTotalGB: 128, RAMAvailableAtInstallGB: 64}
	totalOnly := hardware.Profile{RAMTotalGB: 32}
	unknown := hardware.Profile{}

	for _, tc := range []struct {
		name     string
		request  float64
		hw       hardware.Profile
		want     float64
		wantNote bool
	}{
		{"disabled by default", 0, standing, 0, false},
		{"a request inside the ceiling passes through", 8, standing, 8, false},
		// The standing figure (64) wins over RAMTotalGB (128): a live or
		// total reading would let the buffer count memory the host does
		// not actually have spare.
		{"clamped to a quarter of the standing figure", 40, standing, 16, true},
		{"falls back to total RAM when no standing figure", 40, totalOnly, 8, true},
		{"refused with no RAM measurement at all", 8, unknown, 0, true},
		{"a sub-GiB buffer is not worth allocating", 0.5, standing, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, note := VLLMKVOffloadingGiB(tc.request, tc.hw)
			if got != tc.want {
				t.Errorf("value = %v, want %v (note=%q)", got, tc.want, note)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q, want non-empty=%v", note, tc.wantNote)
			}
		})
	}
}
