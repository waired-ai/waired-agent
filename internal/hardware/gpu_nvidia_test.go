package hardware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNvidiaSMICSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []GPU
	}{
		{
			name: "single Blackwell",
			in:   "NVIDIA RTX PRO 4000 Blackwell, 24467, 595.58.03, 12.0, GPU-abc123\n",
			want: []GPU{{
				Vendor: "nvidia", Model: "NVIDIA RTX PRO 4000 Blackwell",
				VRAMTotalMB: 24467, DriverVersion: "595.58.03",
				ComputeCap: "12.0", UUID: "GPU-abc123",
			}},
		},
		{
			name: "two GPUs",
			in: "NVIDIA L4, 24576, 550.54.15, 8.9, GPU-aaa\n" +
				"NVIDIA L40S, 49152, 550.54.15, 8.9, GPU-bbb\n",
			want: []GPU{
				{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 24576, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-aaa"},
				{Vendor: "nvidia", Model: "NVIDIA L40S", VRAMTotalMB: 49152, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-bbb"},
			},
		},
		{
			name: "trailing blank line tolerated",
			in:   "NVIDIA T4, 16384, 535.86.10, 7.5, GPU-x\n\n",
			want: []GPU{
				{Vendor: "nvidia", Model: "NVIDIA T4", VRAMTotalMB: 16384, DriverVersion: "535.86.10", ComputeCap: "7.5", UUID: "GPU-x"},
			},
		},
	}
	// The 5-field shape above is what the parser accepted before
	// waired-agent#69 and still must: nvidiaSMIQueryBasic's retry and
	// any caller asking for fewer fields go through the same code.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidiaSMICSV(tc.in, 5)
			if err != nil {
				t.Fatalf("parseNvidiaSMICSV: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("GPU[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Product contract: the basic query's 3 columns parse into the same
// record shape, minus the two fields it does not ask for. This is the
// retry path for drivers that reject compute_cap and exit non-zero —
// under the old parser that exit read as "this host has no GPU" (#67).
// TestParseNvidiaSMICSV_FreeFieldSet covers the 6-field shape
// nvidiaSMIQueryFull now asks for (waired-agent#69). memory.free is
// APPENDED, so this also pins that adding it moved none of the existing
// indices — a regression here would silently reassign driver_version,
// compute_cap or the UUID.
func TestParseNvidiaSMICSV_FreeFieldSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want GPU
	}{
		{
			// The waired-agent#69 host: 8 GB card, ~2 GB held by the
			// display before any model loads.
			"a busy card reports what is left",
			"NVIDIA GeForce RTX 3060 Ti, 8192, 580.65, 8.6, GPU-abc, 6144\n",
			GPU{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti", VRAMTotalMB: 8192,
				VRAMFreeMB: 6144, DriverVersion: "580.65", ComputeCap: "8.6", UUID: "GPU-abc"},
		},
		{
			"an idle card reports its whole capacity",
			"NVIDIA L4, 23034, 550.54.15, 8.9, GPU-aaa, 23034\n",
			GPU{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034,
				VRAMFreeMB: 23034, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-aaa"},
		},
		{
			// An unreadable free figure must not cost us the DEVICE.
			// Losing a card over a soft field is the #67 direction; the
			// budget simply falls back to the total.
			"an unparseable free figure keeps the device",
			"NVIDIA L4, 23034, 550.54.15, 8.9, GPU-aaa, [N/A]\n",
			GPU{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034,
				VRAMFreeMB: 0, DriverVersion: "550.54.15", ComputeCap: "8.9", UUID: "GPU-aaa"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidiaSMICSV(tc.in, 6)
			if err != nil {
				t.Fatalf("parseNvidiaSMICSV: %v", err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("got %+v, want [%+v]", got, tc.want)
			}
		})
	}
}

func TestParseNvidiaSMICSV_BasicFieldSet(t *testing.T) {
	got, err := parseNvidiaSMICSV("NVIDIA GeForce RTX 3060 Ti, 8192, 460.89\n", 3)
	if err != nil {
		t.Fatalf("parseNvidiaSMICSV: %v", err)
	}
	want := []GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti", VRAMTotalMB: 8192, DriverVersion: "460.89"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseNvidiaSMICSV_Malformed(t *testing.T) {
	cases := []string{
		"too, few, fields\n",
		"NVIDIA T4, not-a-number, 535.86.10, 7.5, GPU-x\n",
	}
	for _, in := range cases {
		if _, err := parseNvidiaSMICSV(in, 5); err == nil {
			t.Errorf("parseNvidiaSMICSV(%q) = nil error, want error", in)
		}
	}
}

// Product contract (#67): $PATH is one step of the chain, not the
// question. The candidate list must reach the driver's own install
// locations on every OS, so a LocalSystem service that inherits no user
// PATH still finds the tool. Table over all three GOOS values per
// CLAUDE.md §Cross-OS parity.
func TestNvidiaSMICandidates(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	winEnv := map[string]string{
		"SystemRoot":   `C:\WINDOWS`,
		"ProgramFiles": `C:\Program Files`,
	}

	cases := []struct {
		name   string
		goos   string
		env    map[string]string
		onPATH string
		want   []string
	}{
		{
			name:   "windows without PATH still reaches System32 and the DriverStore",
			goos:   "windows",
			env:    winEnv,
			onPATH: "",
			want: []string{
				`C:\WINDOWS\System32\nvidia-smi.exe`,
				`C:\WINDOWS\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
				`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			},
		},
		{
			name:   "windows PATH hit is tried first and not repeated",
			goos:   "windows",
			env:    winEnv,
			onPATH: `C:\WINDOWS\System32\nvidia-smi.exe`,
			want: []string{
				`C:\WINDOWS\System32\nvidia-smi.exe`,
				`C:\WINDOWS\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
				`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			},
		},
		{
			name: "windows with a 32-bit ProgramFiles adds the native one too",
			goos: "windows",
			env: map[string]string{
				"SystemRoot":   `C:\WINDOWS`,
				"ProgramFiles": `C:\Program Files (x86)`,
				"ProgramW6432": `C:\Program Files`,
			},
			want: []string{
				`C:\WINDOWS\System32\nvidia-smi.exe`,
				`C:\WINDOWS\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
				`C:\Program Files (x86)\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
				`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			},
		},
		{
			name: "linux covers the distro paths and the WSL mount",
			goos: "linux",
			want: []string{
				"/usr/bin/nvidia-smi",
				"/usr/local/bin/nvidia-smi",
				"/opt/nvidia/bin/nvidia-smi",
				"/usr/lib/wsl/lib/nvidia-smi",
			},
		},
		{
			name: "darwin has no NVIDIA driver to look for",
			goos: "darwin",
			want: nil,
		},
		{
			name: "the explicit override wins on every OS",
			goos: "darwin",
			env:  map[string]string{nvidiaSMIEnvOverride: "/opt/custom/nvidia-smi"},
			want: []string{"/opt/custom/nvidia-smi"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nvidiaSMICandidates(tc.goos, env(tc.env), tc.onPATH)
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("candidate[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestResolveNvidiaSMI(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "nvidia-smi")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("override resolves", func(t *testing.T) {
		got, ok := resolveNvidiaSMI("linux", env(map[string]string{nvidiaSMIEnvOverride: real}), "")
		if !ok || got != real {
			t.Errorf("resolveNvidiaSMI = (%q, %v), want (%q, true)", got, ok, real)
		}
	})

	t.Run("a candidate that does not exist is skipped", func(t *testing.T) {
		missing := filepath.Join(dir, "absent", "nvidia-smi")
		got, ok := resolveNvidiaSMI("darwin", env(map[string]string{nvidiaSMIEnvOverride: missing}), real)
		if !ok || got != real {
			t.Errorf("resolveNvidiaSMI = (%q, %v), want the PATH hit %q", got, ok, real)
		}
	})

	t.Run("a directory is not an executable", func(t *testing.T) {
		if _, ok := resolveNvidiaSMI("darwin", env(map[string]string{nvidiaSMIEnvOverride: dir}), ""); ok {
			t.Error("resolveNvidiaSMI matched a directory")
		}
	})

	// The Windows DriverStore candidate is a glob (the driver-version
	// directory is not predictable), so pattern expansion is part of the
	// contract. Exercised through the override so the case is portable.
	t.Run("glob candidates expand", func(t *testing.T) {
		store := filepath.Join(dir, "nv_dispi.inf_amd64_1234")
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		stored := filepath.Join(store, "nvidia-smi")
		if err := os.WriteFile(stored, []byte("x"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		pattern := filepath.Join(dir, "nv*", "nvidia-smi")
		got, ok := resolveNvidiaSMI("windows", env(map[string]string{nvidiaSMIEnvOverride: pattern}), "")
		if !ok || got != stored {
			t.Errorf("resolveNvidiaSMI = (%q, %v), want (%q, true)", got, ok, stored)
		}
	})

	t.Run("nothing anywhere", func(t *testing.T) {
		if _, ok := resolveNvidiaSMI("darwin", env(nil), ""); ok {
			t.Error("resolveNvidiaSMI reported a tool with no candidates")
		}
	})
}

// THE #67 REGRESSION BAR, product contract. Detection must distinguish
// "this host has no NVIDIA GPU" from "something is here and I could not
// read it". The old code collapsed the two into a silent "no GPU", which
// sized the model picker for RAM, labelled the backend `cpu`, and left
// an 8 GB card idle with no error anywhere.
func TestClassifyNvidia(t *testing.T) {
	card := GPU{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti", VRAMTotalMB: 8192}
	smiFailed := errors.New("nvidia-smi: exit status 9")

	cases := []struct {
		name     string
		smi      nvidiaSMIResult
		fb       nvidiaFallbackResult
		wantGPUs int
		wantCUDA bool
		wantErr  bool
		// wantErrHas, when set, must appear in the error text — the
		// operator-facing half of the diagnostic.
		wantErrHas string
	}{
		{
			name:     "nvidia-smi answered: authoritative, quiet",
			smi:      nvidiaSMIResult{GPUs: []GPU{card}, Ran: true},
			wantGPUs: 1, wantCUDA: true,
		},
		{
			name: "nvidia-smi answered empty: a host with no NVIDIA card, quiet",
			smi:  nvidiaSMIResult{Ran: true},
			// A stale registry entry must not resurrect a removed card.
			fb:       nvidiaFallbackResult{GPUs: []GPU{card}, AdapterSeen: true, Source: "registry"},
			wantGPUs: 0, wantCUDA: false,
		},
		{
			name:     "no nvidia-smi, NVML found the card: detected, with a warning",
			smi:      nvidiaSMIResult{Err: errNvidiaSMIAbsent},
			fb:       nvidiaFallbackResult{GPUs: []GPU{card}, AdapterSeen: true, Source: "nvml"},
			wantGPUs: 1, wantCUDA: true, wantErr: true, wantErrHas: "nvml",
		},
		{
			name: "device found with no VRAM figure: detected, and the gap is named",
			smi:  nvidiaSMIResult{Err: errNvidiaSMIAbsent},
			fb: nvidiaFallbackResult{
				GPUs:        []GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti"}},
				AdapterSeen: true, Source: "procfs",
			},
			wantGPUs: 1, wantCUDA: true, wantErr: true, wantErrHas: "VRAM unknown",
		},
		{
			name:     "adapter present but unenumerable: LOUD, never silent CPU",
			smi:      nvidiaSMIResult{Err: errNvidiaSMIAbsent},
			fb:       nvidiaFallbackResult{AdapterSeen: true, Source: "display-adapter registry"},
			wantGPUs: 0, wantCUDA: false, wantErr: true, wantErrHas: nvidiaSMIEnvOverride,
		},
		{
			name:     "nvidia-smi found and failed: a failure, not an absence",
			smi:      nvidiaSMIResult{Err: smiFailed},
			wantGPUs: 0, wantCUDA: false, wantErr: true, wantErrHas: "exit status 9",
		},
		{
			name:     "no tool, no driver, no adapter: genuinely CPU-only",
			smi:      nvidiaSMIResult{Err: errNvidiaSMIAbsent},
			wantGPUs: 0, wantCUDA: false, wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpus, accel, err := classifyNvidia(tc.smi, tc.fb)
			if len(gpus) != tc.wantGPUs {
				t.Errorf("gpus = %+v, want %d", gpus, tc.wantGPUs)
			}
			if accel.CUDA != tc.wantCUDA {
				t.Errorf("accel.CUDA = %v, want %v", accel.CUDA, tc.wantCUDA)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErrHas != "" && !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantErrHas)
			}
		})
	}
}

// THE #67 REGRESSION BAR, end to end: with NOTHING on $PATH — the
// Windows LocalSystem service's situation — the resolved tool is still
// executed and its output parsed. The fake tool is this test binary
// (see TestMain in profiler_exec_test.go), which is the one way to spawn
// a real process identically on all three OSes.
func TestNvidiaSMIProbe_EmptyPATH(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	t.Run("full field set", func(t *testing.T) {
		t.Setenv("PATH", "")
		// Six fields since waired-agent#69: memory.free is appended
		// last, so every earlier index is where it was. This is the
		// waired-agent#69 host — an 8 GB card with ~2 GB already held by
		// the display.
		t.Setenv(fakeEngineEnv, "NVIDIA GeForce RTX 3060 Ti, 8192, 580.65, 8.6, GPU-abc, 6144")
		t.Setenv(nvidiaSMIEnvOverride, self)

		got := nvidiaSMIProbe(context.Background())
		if !got.Ran {
			t.Fatalf("Ran = false, err = %v", got.Err)
		}
		want := GPU{
			Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti", VRAMTotalMB: 8192,
			VRAMFreeMB: 6144, DriverVersion: "580.65", ComputeCap: "8.6", UUID: "GPU-abc",
		}
		if len(got.GPUs) != 1 || got.GPUs[0] != want {
			t.Errorf("GPUs = %+v, want [%+v]", got.GPUs, want)
		}
	})

	// A driver too old for compute_cap makes the full query fail; the
	// retry with the basic field set still yields the card.
	t.Run("falls back to the basic field set", func(t *testing.T) {
		t.Setenv("PATH", "")
		t.Setenv(fakeEngineEnv, "NVIDIA GeForce GTX 1080, 8192, 440.33")
		t.Setenv(nvidiaSMIEnvOverride, self)

		got := nvidiaSMIProbe(context.Background())
		if !got.Ran {
			t.Fatalf("Ran = false, err = %v", got.Err)
		}
		want := GPU{Vendor: "nvidia", Model: "NVIDIA GeForce GTX 1080", VRAMTotalMB: 8192, DriverVersion: "440.33"}
		if len(got.GPUs) != 1 || got.GPUs[0] != want {
			t.Errorf("GPUs = %+v, want [%+v]", got.GPUs, want)
		}
	})

	// "No tool anywhere" is asserted at the resolution layer
	// (TestResolveNvidiaSMI) rather than here: this probe reads the host's
	// real GOOS, so on a machine that genuinely has the driver installed
	// the chain correctly finds it in a well-known location and there is
	// no absence to observe.
}
