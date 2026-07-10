package hardware

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// detectNvidia probes for NVIDIA GPUs by shelling out to `nvidia-smi`.
// The CSV output is parsed by parseNvidiaSMICSV. When nvidia-smi is
// not on PATH the function returns (nil, zero, nil) — i.e. "no GPU
// detected" is not treated as an error so the calling Profile build
// still succeeds on CPU-only hosts. A non-zero exit or unparseable
// output IS an error and is surfaced via Profile.Errors.
//
// Multi-GPU hosts populate the slice in nvidia-smi's reporting order
// (which is GPU index order). The model selectors still consult only
// GPUs[0] (engine-aware multi-GPU VRAM budgeting is a follow-up), but
// the vLLM serving path shards across identical NVIDIA GPUs via
// router.VLLMTensorParallelSize.
func detectNvidia(ctx context.Context) ([]GPU, Accelerators, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, Accelerators{}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx,
		"nvidia-smi",
		"--query-gpu=name,memory.total,driver_version,compute_cap,uuid",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, Accelerators{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	gpus, err := parseNvidiaSMICSV(string(out))
	if err != nil {
		return nil, Accelerators{}, fmt.Errorf("parse nvidia-smi: %w", err)
	}
	accel := Accelerators{CUDA: len(gpus) > 0}
	return gpus, accel, nil
}

// parseNvidiaSMICSV parses the `--query-gpu=name,memory.total,driver_version,compute_cap,uuid`
// CSV output (with `--format=csv,noheader,nounits`). Each non-empty
// line must have 5 comma-separated fields; memory.total is reported in
// MiB by `nounits` and stored verbatim as VRAMTotalMB.
//
// Whitespace around commas is tolerated (nvidia-smi inserts a single
// space after each comma in CSV output). Blank trailing lines are
// skipped.
func parseNvidiaSMICSV(s string) ([]GPU, error) {
	var out []GPU
	for i, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("line %d: expected 5 CSV fields, got %d (%q)", i+1, len(fields), line)
		}
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		mb, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: memory.total = %q: %w", i+1, fields[1], err)
		}
		out = append(out, GPU{
			Vendor:        "nvidia",
			Model:         fields[0],
			VRAMTotalMB:   mb,
			DriverVersion: fields[2],
			ComputeCap:    fields[3],
			UUID:          fields[4],
		})
	}
	return out, nil
}
