package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPlacementFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "llamacpp", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestParseLlamaPlacement reads real llama.cpp load transcripts. The CUDA
// fixture is one load captured on a 24 GB card (qwen3.8-27b MTP-Q4 at
// 200,704 tokens with a q4_0 KV cache, ollama 0.33.3; the file keeps the
// lines the parser reads and drops the rest). The Vulkan one is the lines
// docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md
// quotes from a Strix Halo load, with the paths replaced. A record of what
// the pinned engine logs, not a contract llama.cpp keeps.
func TestParseLlamaPlacement(t *testing.T) {
	t.Run("partial offload on a discrete card", func(t *testing.T) {
		p, ok := ParseLlamaPlacement(readPlacementFixture(t, "cuda-27b-mtp-q4kv-63of66.log"))
		if !ok {
			t.Fatal("no placement parsed")
		}
		if p.OffloadedLayers != 63 || p.TotalLayers != 66 || p.CPULayers() != 3 {
			t.Errorf("layers = %d/%d (cpu %d), want 63/66 (cpu 3)", p.OffloadedLayers, p.TotalLayers, p.CPULayers())
		}
		if p.DeviceWeightsMiB != 14617.70 || p.HostWeightsMiB != 1403.76 {
			t.Errorf("weights device %.2f host %.2f, want 14617.70 / 1403.76", p.DeviceWeightsMiB, p.HostWeightsMiB)
		}
		if p.ContextCells != 200704 {
			t.Errorf("n_ctx = %d, want the main context's 200704", p.ContextCells)
		}
		// The main cache is q4_0; the MTP draft context's f16 cache logged
		// after it must not be read as the main one.
		if p.KVCacheType != "q4_0" {
			t.Errorf("KV type = %q, want q4_0", p.KVCacheType)
		}
		if p.FitProjectedMiB != 21723 || p.FitFreeMiB != 23002 || p.FitShortMiB != 658 {
			t.Errorf("fit = %d vs %d short %d, want 21723 vs 23002 short 658", p.FitProjectedMiB, p.FitFreeMiB, p.FitShortMiB)
		}
		if !strings.HasSuffix(p.ModelPath, "sha256-f5f1dd8920d417aac2718b0bda3403da274301efdd6760b4f0f4b864ff2ad57d") {
			t.Errorf("model path = %q", p.ModelPath)
		}
	})

	t.Run("every layer offloaded while a third of the weights are CPU_Mapped", func(t *testing.T) {
		p, ok := ParseLlamaPlacement(readPlacementFixture(t, "vulkan-flash-next-49of49-cpu-mapped.log"))
		if !ok {
			t.Fatal("no placement parsed")
		}
		if p.CPULayers() != 0 {
			t.Errorf("cpu layers = %d, want 0", p.CPULayers())
		}
		// Vulkan_Host (416.80) and CPU_Mapped (27465.95) are both system RAM.
		if got, want := p.HostWeightsMiB, 416.80+27465.95; got < want-0.01 || got > want+0.01 {
			t.Errorf("host weights = %.2f, want %.2f", got, want)
		}
		if p.DeviceWeightsMiB != 47322.20 {
			t.Errorf("device weights = %.2f, want 47322.20", p.DeviceWeightsMiB)
		}
		if p.FitShortMiB != 0 {
			t.Errorf("fit short = %d, want 0 (no changes needed)", p.FitShortMiB)
		}
	})

	t.Run("the last load wins", func(t *testing.T) {
		older := strings.Replace(readPlacementFixture(t, "cuda-27b-mtp-q4kv-63of66.log"), "offloaded 63/66", "offloaded 50/66", 1)
		p, ok := ParseLlamaPlacement(older + readPlacementFixture(t, "vulkan-flash-next-49of49-cpu-mapped.log"))
		if !ok || p.TotalLayers != 49 {
			t.Fatalf("placement = %+v ok=%v, want the later 49-layer load", p, ok)
		}
	})

	t.Run("a runner that has not finished loading is no evidence", func(t *testing.T) {
		log := readPlacementFixture(t, "cuda-27b-mtp-q4kv-63of66.log") +
			`time=2026-09-12T21:00:00.000Z level=INFO source=llama_server.go:433 msg="starting llama-server" cmd="llama-server -c 200704 -np 1"` + "\n" +
			"common_params_fit_impl: projected to use 21723 MiB of device memory vs. 23002 MiB of free device memory\n"
		if p, ok := ParseLlamaPlacement(log); ok {
			t.Fatalf("placement = %+v, want no evidence while the newest runner has no offload line", p)
		}
	})

	t.Run("nothing to read", func(t *testing.T) {
		for _, tail := range []string{"", "time=… level=INFO msg=\"listening\"\n", EngineLogTruncationMarker} {
			if _, ok := ParseLlamaPlacement(tail); ok {
				t.Errorf("ParseLlamaPlacement(%q) ok, want no evidence", tail)
			}
		}
	})

	t.Run("everything on the CPU", func(t *testing.T) {
		log := strings.NewReplacer(
			"offloaded 63/66", "offloaded 0/66",
			"CPU_Mapped model buffer size =  1403.76", "CPU_Mapped model buffer size = 16021.46",
			"load_tensors:        CUDA0 model buffer size = 14617.70 MiB\n", "",
		).Replace(readPlacementFixture(t, "cuda-27b-mtp-q4kv-63of66.log"))
		p, ok := ParseLlamaPlacement(log)
		if !ok || p.OffloadedLayers != 0 || p.DeviceWeightsMiB != 0 || p.HostWeightsMiB != 16021.46 {
			t.Fatalf("placement = %+v ok=%v, want 0/66 with every weight in system RAM", p, ok)
		}
	})
}
