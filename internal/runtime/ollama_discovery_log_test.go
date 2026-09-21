package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func readDiscoveryFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ollama", "discovery", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The device block ollama logs at start (waired-agent#1513), read from
// fixtures whose device lines are the engine's own output (provenance on
// each fixture's first line).
func TestParseInferenceCompute(t *testing.T) {
	t.Run("CUDA", func(t *testing.T) {
		got, ok := ParseInferenceCompute(readDiscoveryFixture(t, "cuda-rtx-pro-4000.log"))
		if !ok || len(got.Devices) != 1 {
			t.Fatalf("= %+v, %v; want one device", got, ok)
		}
		d := got.Devices[0]
		if d.Library != "CUDA" || d.Compute != "12.0" || d.Description != "NVIDIA RTX PRO 4000 Blackwell" ||
			d.Type != "discrete" || d.Total != "23.5 GiB" {
			t.Errorf("device = %+v", d)
		}
	})
	t.Run("ROCm, CRLF", func(t *testing.T) {
		got, ok := ParseInferenceCompute(readDiscoveryFixture(t, "rocm-strix-halo.log"))
		if !ok || len(got.Devices) != 1 {
			t.Fatalf("= %+v, %v; want one device", got, ok)
		}
		if d := got.Devices[0]; d.Library != "ROCm" || d.Compute != "gfx1151" || d.Type != "iGPU" ||
			d.Description != "AMD Radeon(TM) 8060S Graphics" {
			t.Errorf("device = %+v", d)
		}
	})

	listening := `time=2026-09-21T00:00:00.000Z level=INFO source=routes.go:2000 msg="Listening on 127.0.0.1:9475 (version 0.34.2)"` + "\n"
	end := `time=2026-09-21T00:00:01.000Z level=INFO source=routes.go:2050 msg="vram-based default context" total_vram="0 B" default_num_ctx=4096` + "\n"
	// Synthetic, from discover/types.go LogDetails and discover/runner.go.
	cpu := `time=2026-09-21T00:00:00.500Z level=INFO source=types.go:50 msg="inference compute" id=cpu library=cpu compute="" name=cpu description=cpu libdirs=ollama driver="" pci_id="" type="" total="31.2 GiB" available="29.8 GiB"` + "\n"
	dropped := `time=2026-09-21T00:00:00.400Z level=INFO source=runner.go:405 msg="dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1" id=0 library=Vulkan compute=0.0 name=Vulkan0 description="AMD Radeon(TM) 780M Graphics" pci_id=0000:c6:00.0` + "\n"

	t.Run("no GPU kept", func(t *testing.T) {
		got, ok := ParseInferenceCompute(listening + dropped + cpu + end)
		if !ok || !got.CPUOnly || len(got.Devices) != 0 || len(got.Dropped) != 1 {
			t.Fatalf("= %+v, %v; want CPU only with one integrated GPU dropped", got, ok)
		}
		if got.Dropped[0].Description != "AMD Radeon(TM) 780M Graphics" {
			t.Errorf("dropped = %+v", got.Dropped[0])
		}
	})
	t.Run("the last start wins", func(t *testing.T) {
		first := readDiscoveryFixture(t, "cuda-rtx-pro-4000.log")
		got, ok := ParseInferenceCompute(first + listening + cpu + end)
		if !ok || !got.CPUOnly || len(got.Devices) != 0 {
			t.Errorf("= %+v, %v; want the second start's CPU-only block", got, ok)
		}
	})
	t.Run("a block without its closing line is not read", func(t *testing.T) {
		if got, ok := ParseInferenceCompute(listening + cpu); ok {
			t.Errorf("= %+v, true; want not ok until the block is complete", got)
		}
	})
	t.Run("no start at all", func(t *testing.T) {
		if _, ok := ParseInferenceCompute(cpu + end); ok {
			t.Error("read a block with no Listening line before it")
		}
	})
	t.Run("an escaped quote in a description", func(t *testing.T) {
		line := `time=x level=INFO msg="inference compute" id=0 library=Vulkan compute=0.0 name=Vulkan0 description="Some \"Special\" GPU" type=discrete total="8.0 GiB"` + "\n"
		got, ok := ParseInferenceCompute(listening + line + end)
		if !ok || len(got.Devices) != 1 || got.Devices[0].Description != `Some "Special" GPU` || got.Devices[0].Total != "8.0 GiB" {
			t.Errorf("= %+v, %v", got, ok)
		}
	})
}

func TestHeadEngineLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := headEngineLog(path, 4); got != "0123" {
		t.Errorf("head = %q, want the first 4 bytes", got)
	}
	if got := headEngineLog(filepath.Join(t.TempDir(), "absent"), 4); got != "" {
		t.Errorf("head of a missing log = %q, want empty", got)
	}
}
