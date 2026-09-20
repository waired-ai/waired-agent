package hardware

import "testing"

// The key must be the same for one machine across operating systems.
// The AMD rows are the two spellings the SAME part reports — Windows
// carries the iGPU in the CPU string and a "+" that Linux omits — and a
// key that differed between them would file one machine's measurements
// under two names, which is what the single shared vocabulary in
// docs/decisions/20260829/1100 existed to prevent.
func TestChipSlug(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		vendor, cpuModel, computeCap string
		want                         string
	}{
		{
			name:   "AMD Strix Halo as Windows reports it",
			vendor: "amd", cpuModel: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S",
			want: "ryzen-ai-max-395",
		},
		{
			name:   "AMD Strix Halo as Linux reports it",
			vendor: "amd", cpuModel: "AMD Ryzen AI Max 395",
			want: "ryzen-ai-max-395",
		},
		{
			name:   "AMD Phoenix, which is a different part and must not collide",
			vendor: "amd", cpuModel: "AMD Ryzen 9 7940HS w/ Radeon 780M Graphics",
			want: "ryzen-9-7940hs",
		},
		{
			name: "Apple M4 Max", vendor: "apple", cpuModel: "Apple M4 Max",
			want: "m4-max",
		},
		{
			name:   "Apple M4, which must not collide with M4 Max",
			vendor: "apple", cpuModel: "Apple M4",
			want: "m4",
		},
		{
			name:   "NVIDIA names itself by compute capability",
			vendor: "nvidia", cpuModel: "AMD Ryzen 9 7950X", computeCap: "12.0",
			want: "sm120",
		},
		{
			name:   "NVIDIA on aarch64, where CPU.Model is empty",
			vendor: "nvidia", cpuModel: "", computeCap: "12.1",
			want: "sm121",
		},
		{
			name:   "NVIDIA with no compute capability reported",
			vendor: "nvidia", cpuModel: "some cpu", computeCap: "",
			want: "unknown",
		},
		{
			name:   "an Intel part falls back to the CPU string",
			vendor: "intel", cpuModel: "Intel(R) Core(TM) Ultra 9 285H",
			want: "intel-r-core-tm-ultra-9-285h",
		},
		{
			name: "nothing to name", vendor: "amd", cpuModel: "",
			want: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChipSlug(tc.vendor, tc.cpuModel, tc.computeCap); got != tc.want {
				t.Errorf("ChipSlug(%q, %q, %q) = %q, want %q",
					tc.vendor, tc.cpuModel, tc.computeCap, got, tc.want)
			}
		})
	}
}

// The two machines that actually hold measurements today must keep
// describing themselves correctly, and a DGX Spark must produce a key
// without anybody adding a line anywhere — which is the whole of
// waired-agent#1455.
func TestHostKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		prof Profile
		want string
	}{
		{
			name: "the reference host (turnspeeds' amd-unified-128gb today)",
			prof: Profile{
				CPU:           CPUInfo{Model: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"},
				UnifiedMemory: true,
				GPUs: []GPU{{
					Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics",
					Integrated: true, IntegratedKnown: true,
				}},
			},
			want: "unified-amd-ryzen-ai-max-395",
		},
		{
			name: "the same host before sudo waired init has taken the reading",
			prof: Profile{
				CPU:           CPUInfo{Model: "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"},
				UnifiedMemory: true,
				GPUs:          []GPU{{Vendor: "amd", Model: "AMD Radeon(TM) 8060S Graphics"}},
			},
			want: "unified-amd-ryzen-ai-max-395",
		},
		{
			name: "the GPU lane (agentgrade's nvidia-24gb-discrete today)",
			prof: Profile{
				CPU:  CPUInfo{Model: "AMD Ryzen 9 7950X 16-Core Processor"},
				GPUs: []GPU{{Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell", ComputeCap: "12.0"}},
			},
			want: "discrete-nvidia-sm120",
		},
		{
			name: "a GB10, which today would need a new row in the old vocabulary",
			prof: Profile{
				CPU: CPUInfo{Model: ""}, // aarch64: /proc/cpuinfo has no model name
				GPUs: []GPU{{
					Vendor: "nvidia", Model: "NVIDIA GB10", ComputeCap: "12.1",
					Integrated: true, IntegratedKnown: true,
				}},
			},
			want: "unified-nvidia-sm121",
		},
		{
			name: "an Apple Silicon host",
			prof: Profile{
				CPU:           CPUInfo{Model: "Apple M4 Max"},
				UnifiedMemory: true,
				GPUs:          []GPU{{Vendor: "apple", Model: "Apple M4 Max", Integrated: true, IntegratedKnown: true}},
			},
			want: "unified-apple-m4-max",
		},
		{
			name: "a host with no accelerator",
			prof: Profile{CPU: CPUInfo{Model: "Intel(R) Xeon(R) Gold 6338"}},
			want: "cpu-none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HostKey(&tc.prof)
			if got != tc.want {
				t.Errorf("HostKey() = %q, want %q", got, tc.want)
			}
			if !ValidHostKey(got) {
				t.Errorf("HostKey() = %q, which its own validator rejects", got)
			}
		})
	}
}

// A known reading overrides the policy flag in both directions. This is
// what lets a GB10 be spelled "unified" while Profile.UnifiedMemory is
// still false for it, which it will be until the budget rules move
// (waired-agent#459).
func TestHostTopologyOf_ReadingBeatsPolicyFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		prof Profile
		want string
	}{
		{
			name: "reading says unified, policy flag has not caught up",
			prof: Profile{GPUs: []GPU{{Vendor: "nvidia", Integrated: true, IntegratedKnown: true}}},
			want: TopologyUnified,
		},
		{
			name: "reading says discrete, policy flag wrongly says unified",
			prof: Profile{
				UnifiedMemory: true,
				GPUs:          []GPU{{Vendor: "amd", Integrated: false, IntegratedKnown: true}},
			},
			want: TopologyDiscrete,
		},
		{
			name: "no reading: the policy flag is the fallback",
			prof: Profile{UnifiedMemory: true, GPUs: []GPU{{Vendor: "amd"}}},
			want: TopologyUnified,
		},
		{
			name: "no reading and no flag",
			prof: Profile{GPUs: []GPU{{Vendor: "nvidia"}}},
			want: TopologyDiscrete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostTopologyOf(&tc.prof); got != tc.want {
				t.Errorf("HostTopologyOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The validator accepts what the derivation produces and rejects what a
// machine name looks like. The rejected rows are the ones
// docs/decisions/20260829/1100 §2 worried about: it declined to tell a
// class from an identifier by pattern because "sv-mag" and
// "apple-unified-64gb" are both lowercase words joined by hyphens. A
// derived key is not in that bind — it has a topology on the front, and
// nothing types it — but the grammar should still say no to the obvious.
func TestValidHostKey(t *testing.T) {
	for _, s := range []string{
		"unified-amd-ryzen-ai-max-395",
		"discrete-nvidia-sm120",
		"unified-apple-m4-max",
		"unified-nvidia-sm121",
		"cpu-none",
		"discrete-nvidia-unknown",
	} {
		if !ValidHostKey(s) {
			t.Errorf("ValidHostKey(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"sv-mag",
		"amd-unified-128gb",      // the old spelling: no topology in front
		"nvidia-24gb-discrete",   // ditto
		"unified",                // one component
		"unified-",               // trailing hyphen
		"Unified-amd-strix",      // upper case
		"unified--amd",           // empty component
		"unified-amd-strix halo", // space
	} {
		if ValidHostKey(s) {
			t.Errorf("ValidHostKey(%q) = true, want false", s)
		}
	}
}
