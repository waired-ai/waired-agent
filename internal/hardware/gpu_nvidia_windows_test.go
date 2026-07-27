//go:build windows

package hardware

import (
	"strings"
	"testing"
)

// The NVML layer must survive a host with no NVIDIA driver at all —
// which is every GitHub-hosted Windows runner. LazyProc.Call PANICS on a
// missing export, so "the DLL is absent" and "the DLL is present but
// older than an entry point we ask for" are both crash risks, not merely
// empty answers. Contract: never panic, and report ok=false rather than
// an empty device list when NVML could not be used, so the registry
// layer below still runs.
func TestNvidiaNVMLDevices_NoCrash(t *testing.T) {
	gpus, ok := nvidiaNVMLDevices()
	if !ok && len(gpus) > 0 {
		t.Errorf("nvidiaNVMLDevices returned %d devices with ok=false", len(gpus))
	}
	for i, g := range gpus {
		if g.Vendor != "nvidia" {
			t.Errorf("GPUs[%d].Vendor = %q, want nvidia", i, g.Vendor)
		}
		if g.Model == "" {
			t.Errorf("GPUs[%d] has no model", i)
		}
		if g.VRAMTotalMB <= 0 {
			t.Errorf("GPUs[%d].VRAMTotalMB = %d, want > 0 (NVML always reports total)", i, g.VRAMTotalMB)
		}
	}
}

// Same bar for the composed fallback, plus the invariant that makes the
// diagnostic usable: a positive result always names where it came from,
// and devices imply an adapter.
func TestNvidiaFallback_RealHostNoCrash(t *testing.T) {
	got := nvidiaFallback(t.Context())
	for i, g := range got.GPUs {
		if g.Vendor != "nvidia" {
			t.Errorf("GPUs[%d].Vendor = %q, want nvidia", i, g.Vendor)
		}
	}
	if len(got.GPUs) > 0 && !got.AdapterSeen {
		t.Error("devices reported without AdapterSeen")
	}
	if (len(got.GPUs) > 0 || got.AdapterSeen) && got.Source == "" {
		t.Error("a positive result carries no Source for the diagnostic")
	}
}

// nvidiaDriverLibraryPresent is what separates "a card is installed"
// from "a card was once installed and its registry entry outlived it".
// On a host with no driver it must be false and must not panic.
func TestNvidiaDriverLibraryPresent_NoCrash(t *testing.T) {
	present := nvidiaDriverLibraryPresent()
	if present {
		// If nvcuda.dll loaded, NVML should normally be usable too. Not
		// asserted as a hard contract (a broken driver install is exactly
		// the case the registry layer exists for) — recorded for the log.
		if _, ok := nvidiaNVMLDevices(); !ok {
			t.Log("nvcuda.dll present but NVML unusable: the registry fallback path is live on this host")
		}
	}
}

// The vendor tag the walk is asked for must be the one it stamps, or an
// NVIDIA adapter would arrive downstream labelled as something else and
// route to the wrong ollama backend.
func TestNvidiaPCIVendorID(t *testing.T) {
	if !strings.EqualFold(nvidiaPCIVendorID, "VEN_10DE") {
		t.Errorf("nvidiaPCIVendorID = %q, want VEN_10DE", nvidiaPCIVendorID)
	}
}
