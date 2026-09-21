package setup

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The inputs read only the GPUs in use: an AMD iGPU the engine leaves
// off by default (#1484) must not bring the ROCm overlay with it, and an
// AMD card in second position must (#1492).
func TestOllamaBackendInputs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prof        hardware.Profile
		want        infruntime.BackendInputs
		wantOverlay bool
	}{
		{
			name: "the Linux fleet host: NVIDIA in use, AMD iGPU set aside",
			prof: hardware.Profile{
				GPUs:       []hardware.GPU{{Vendor: "nvidia"}},
				UnusedGPUs: []hardware.UnusedGPU{{GPU: hardware.GPU{Vendor: "amd", GFXTarget: "gfx1036"}}},
			},
			want: infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"},
		},
		{
			name:        "an AMD card second",
			prof:        hardware.Profile{GPUs: []hardware.GPU{{Vendor: "NVIDIA"}, {Vendor: "amd", GFXTarget: "gfx1100"}}},
			want:        infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia", AMDGPU: true},
			wantOverlay: true,
		},
		{
			name: "linux strix halo",
			prof: hardware.Profile{GPUs: []hardware.GPU{{Vendor: "amd", GFXTarget: "gfx1151"}}},
			want: infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true},
			// Owner ruling: no special rule on Linux; the engine gets ROCm.
			wantOverlay: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OllamaBackendInputs("linux", tc.prof)
			if got != tc.want {
				t.Errorf("inputs = %+v, want %+v", got, tc.want)
			}
			if o := infruntime.WantsROCmOverlay(got); o != tc.wantOverlay {
				t.Errorf("WantsROCmOverlay = %v, want %v", o, tc.wantOverlay)
			}
		})
	}
}
