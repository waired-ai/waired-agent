package hardware

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// detectAMD probes for AMD GPUs by shelling out to `rocm-smi`. The
// CSV output is parsed by parseROCmSMICSV. When rocm-smi is not on
// PATH the function falls back to amdWindowsFallback (which probes
// the registry on Windows and is a no-op stub on Linux/Darwin) — see
// gpu_amd_windows.go / gpu_amd_other.go.
//
// Falling back to the registry is important for the Windows + Ollama
// case: Ollama ships its own HIP runtime and most desktop users with
// an AMD GPU do not install the ROCm/HIP SDK separately, so rocm-smi
// is usually absent. The registry probe yields adapter name, driver
// version, and (when the driver populates HardwareInformation.
// qwMemorySize) VRAM total. Adapters where VRAM remains unreadable
// trigger a soft warning via the returned error.
//
// As with detectNvidia, "no device detected" is not an error —
// nil/zero/nil is returned so CPU-only hosts build a Profile cleanly.
func detectAMD(ctx context.Context) ([]GPU, Accelerators, error) {
	if _, err := exec.LookPath("rocm-smi"); err == nil {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "rocm-smi",
			"--showproductname",
			"--showmeminfo", "vram",
			"--showdriverversion",
			"--csv",
		).Output()
		if err != nil {
			return nil, Accelerators{}, fmt.Errorf("rocm-smi: %w", err)
		}
		gpus, err := parseROCmSMICSV(string(out))
		if err != nil {
			return nil, Accelerators{}, fmt.Errorf("parse rocm-smi: %w", err)
		}
		if len(gpus) > 0 {
			return gpus, Accelerators{ROCm: true}, nil
		}
		return nil, Accelerators{}, nil
	}
	gpus := amdWindowsFallback(ctx)
	if len(gpus) == 0 {
		return nil, Accelerators{}, nil
	}
	missing := 0
	for _, g := range gpus {
		if g.VRAMTotalMB == 0 {
			missing++
		}
	}
	if missing > 0 {
		return gpus, Accelerators{ROCm: true},
			fmt.Errorf("gpu(amd): VRAM unknown for %d of %d adapter(s) via registry; install ROCm/HIP SDK or update AMD driver for accurate VRAM", missing, len(gpus))
	}
	return gpus, Accelerators{ROCm: true}, nil
}

// parseROCmSMICSV parses rocm-smi's --csv output. Unlike nvidia-smi
// (where we control the column set via --query-gpu= and parse
// positionally), rocm-smi's columns vary by ROCm version and flag
// combination — so we use the first non-empty line as a header and
// look fields up by case-insensitive name.
//
// Model resolution order: "Card model" → "Card series" → "Marketing
// Name" → "Card SKU". VRAM bytes from "VRAM Total Memory (B)" or
// "VRAM Total Memory"; driver from "Driver version"; device id from
// "device". Missing columns degrade gracefully (empty field, no
// error). A row whose column count differs from the header IS an
// error, as is a non-integer bytes value.
func parseROCmSMICSV(s string) ([]GPU, error) {
	var (
		header []string
		out    []GPU
	)
	for i, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		if header == nil {
			header = make([]string, len(fields))
			for j, f := range fields {
				header[j] = strings.ToLower(f)
			}
			continue
		}
		if len(fields) != len(header) {
			return nil, fmt.Errorf("line %d: column count %d does not match header (%d) (%q)", i+1, len(fields), len(header), line)
		}
		row := make(map[string]string, len(header))
		for j, h := range header {
			row[h] = fields[j]
		}
		gpu := GPU{
			Vendor:        "amd",
			Model:         firstNonEmpty(row["card model"], row["card series"], row["marketing name"], row["card sku"]),
			DriverVersion: row["driver version"],
			UUID:          row["device"],
		}
		if vramStr := firstNonEmpty(row["vram total memory (b)"], row["vram total memory"]); vramStr != "" {
			b, err := strconv.ParseInt(vramStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: vram bytes = %q: %w", i+1, vramStr, err)
			}
			gpu.VRAMTotalMB = int(b / (1024 * 1024))
		}
		out = append(out, gpu)
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
