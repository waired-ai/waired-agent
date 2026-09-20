package hardware

import (
	"strings"
	"testing"
)

// A field nvidia-smi declines to answer is part of the CSV format, not a
// malformed line: the manual says "any unsupported data is indicated by
// a N/A in the output". Under --format=csv the sentinel is bracketed.
//
// This is a record of nvidia-smi's documented behaviour, not a product
// contract — the set of wordings is NVIDIA's to change, which is why the
// predicate matches the shape rather than a list.
func TestNvidiaSMIUnavailable(t *testing.T) {
	for _, s := range []string{
		"[N/A]", "[Not Supported]", "[Insufficient Permissions]", "[Unknown Error]",
		"N/A", "n/a",
	} {
		if !nvidiaSMIUnavailable(s) {
			t.Errorf("nvidiaSMIUnavailable(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "0", "24564", "NVIDIA GB10", "12.1", "580.105.08",
		// A garbled figure is NOT the driver declining to give one.
		"banana", "24,564", "[", "]",
	} {
		if nvidiaSMIUnavailable(s) {
			t.Errorf("nvidiaSMIUnavailable(%q) = true, want false", s)
		}
	}
}

// The regression: a unified-memory NVIDIA part answers "[N/A]" to both
// memory columns because it has no separate framebuffer. Before
// waired-agent#459 that failed the parse, which failed the whole query
// — including the retry, which asks for memory.total too — so the host
// fell through to the /proc/driver/nvidia/gpus fallback and lost the
// device name, driver version and compute capability along with the
// memory figure.
func TestParseNvidiaSMICSV_UnifiedMemoryPartKeepsEverythingElse(t *testing.T) {
	const line = "NVIDIA GB10, [N/A], 580.105.08, 12.1, GPU-1234abcd-0000-0000-0000-00000000abcd, [N/A]"
	gpus, err := parseNvidiaSMICSV(line, 6)
	if err != nil {
		t.Fatalf("parseNvidiaSMICSV: %v — a device that will not report memory must not fail the query", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("got %d devices, want 1", len(gpus))
	}
	g := gpus[0]
	if g.Model != "NVIDIA GB10" {
		t.Errorf("Model = %q, want the name it did answer with", g.Model)
	}
	if g.DriverVersion != "580.105.08" {
		t.Errorf("DriverVersion = %q", g.DriverVersion)
	}
	if g.ComputeCap != "12.1" {
		t.Errorf("ComputeCap = %q — this is what names the chip in a host key", g.ComputeCap)
	}
	if g.VRAMTotalMB != 0 || g.VRAMFreeMB != 0 {
		t.Errorf("VRAM total/free = %d/%d, want 0/0 (unknown, not invented)", g.VRAMTotalMB, g.VRAMFreeMB)
	}
}

// A sentinel must never reach a consumer as if it were a value. A
// ComputeCap of "[N/A]" would become a chip slug in a host key, and a
// Model of "[N/A]" would be printed to the operator as a device name.
func TestParseNvidiaSMICSV_SentinelsDoNotLeakAsText(t *testing.T) {
	const line = "[N/A], 24564, [N/A], [N/A], [N/A], 24000"
	gpus, err := parseNvidiaSMICSV(line, 6)
	if err != nil {
		t.Fatalf("parseNvidiaSMICSV: %v", err)
	}
	g := gpus[0]
	for name, got := range map[string]string{
		"Model": g.Model, "DriverVersion": g.DriverVersion,
		"ComputeCap": g.ComputeCap, "UUID": g.UUID,
	} {
		if got != "" {
			t.Errorf("%s = %q, want empty — a sentinel is not a value", name, got)
		}
	}
	if g.VRAMTotalMB != 24564 || g.VRAMFreeMB != 24000 {
		t.Errorf("the figures it DID answer were lost: total=%d free=%d", g.VRAMTotalMB, g.VRAMFreeMB)
	}
}

// Tolerating the sentinel must not tolerate garbage. A memory total that
// is neither a number nor nvidia-smi's own "would not answer" is a line
// this parser does not understand, and saying so is better than
// inventing a 0.
func TestParseNvidiaSMICSV_GarbledMemoryStillFails(t *testing.T) {
	_, err := parseNvidiaSMICSV("NVIDIA RTX 4090, banana, 550.1", 3)
	if err == nil {
		t.Fatal("parse succeeded on a garbled memory total; want an error")
	}
	if !strings.Contains(err.Error(), "memory.total") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

// A device kept without a memory figure gets a soft warning, on the
// "data + warning" contract the fallback path already uses: something
// downstream is about to size a budget from a figure that is missing,
// and silence would be the wrong answer.
func TestNvidiaFromSMI_WarnsWhenAMemoryTotalIsMissing(t *testing.T) {
	t.Run("a device with no memory total", func(t *testing.T) {
		gpus, accel, err := nvidiaFromSMI([]GPU{
			{Vendor: "nvidia", Model: "NVIDIA GB10", ComputeCap: "12.1"},
		})
		if len(gpus) != 1 {
			t.Fatalf("the device was dropped: %+v", gpus)
		}
		if !accel.CUDA {
			t.Error("CUDA = false; the driver answered, so it is present")
		}
		if err == nil {
			t.Fatal("no warning; a missing budget must be said out loud")
		}
		for _, want := range []string{"no memory total", "no VRAM budget is applied"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("warning = %v, want it to mention %q", err, want)
			}
		}
	})
	t.Run("every device answered", func(t *testing.T) {
		_, _, err := nvidiaFromSMI([]GPU{{Vendor: "nvidia", VRAMTotalMB: 24564}})
		if err != nil {
			t.Errorf("warning = %v, want none", err)
		}
	})
	t.Run("no devices at all", func(t *testing.T) {
		gpus, accel, err := nvidiaFromSMI(nil)
		if len(gpus) != 0 || accel.CUDA || err != nil {
			t.Errorf("got (%+v, %+v, %v), want empty and quiet", gpus, accel, err)
		}
	})
}
