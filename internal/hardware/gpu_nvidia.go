package hardware

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// nvidiaSMITimeout bounds one nvidia-smi invocation. It is deliberately
// generous: on Windows the first call after boot pays NVML's cold
// initialisation and has been observed taking several seconds, and the
// former 3s bound turned that into "no GPU on this host" (#67). The
// profile is cached (30s TTL) and in practice detected once per boot, so
// the worst case costs nothing that repeats.
const nvidiaSMITimeout = 10 * time.Second

// nvidiaSMIEnvOverride names an explicit nvidia-smi to use, ahead of
// every other candidate. Mirrors $WAIRED_OLLAMA_BINARY: an escape hatch
// for hosts whose driver put the tool somewhere this chain does not know
// about, and the actionable half of the "nvidia-smi was not found"
// diagnostic.
const nvidiaSMIEnvOverride = "WAIRED_NVIDIA_SMI"

// nvidia-smi CSV field sets. The full set is what we want; the basic set
// is the retry for drivers that reject a field and exit non-zero rather
// than omitting it (compute_cap arrived in the 45x series).
const (
	nvidiaSMIQueryFull  = "name,memory.total,driver_version,compute_cap,uuid"
	nvidiaSMIQueryBasic = "name,memory.total,driver_version"
)

// errNvidiaSMIAbsent means no nvidia-smi was found anywhere in the
// resolution chain — distinct from "it was found and failed", because
// only the latter says anything about the host's driver.
var errNvidiaSMIAbsent = errors.New("nvidia-smi not found")

// nvidiaSMIResult is the outcome of the nvidia-smi layer.
//
// Ran is what makes an EMPTY GPUs slice authoritative: the vendor's own
// tool executed and reported no device, so the fallbacks must not invent
// one from a stale display-adapter entry left behind by a card that has
// since been removed. When Ran is false the layer has no opinion at all,
// and Err says which way it failed.
type nvidiaSMIResult struct {
	GPUs []GPU
	Ran  bool
	Err  error
}

// nvidiaFallbackResult is what the per-OS probe found once nvidia-smi
// could not answer. It separates "here are the devices" from "something
// NVIDIA-shaped is present here", because the second is enough to make
// the miss LOUD even when nothing can be enumerated — which is the part
// of #67 that mattered most: the host ran on CPU with no error anywhere.
type nvidiaFallbackResult struct {
	// GPUs are devices this probe could actually enumerate. VRAMTotalMB
	// may be 0 when the source does not carry it (the Linux procfs path);
	// consumers degrade to "budget unknown" rather than to "no GPU".
	GPUs []GPU
	// AdapterSeen is true when an NVIDIA display adapter exists on this
	// host even if no device could be enumerated.
	AdapterSeen bool
	// Source names where GPUs / AdapterSeen came from ("nvml",
	// "registry", "procfs", "sysfs"), for the diagnostic text only.
	Source string
}

// detectNvidia reports the host's NVIDIA GPUs.
//
// The question it asks is "is an NVIDIA driver alive on this host", NOT
// "is nvidia-smi on $PATH". Those are different questions and they split
// on exactly the hosts this matters for: a Windows LocalSystem service
// inherits no user PATH, so the CLI can be missing on a machine whose
// driver is perfectly healthy — and the old PATH-only probe returned
// "no GPU" as a NON-error there, which sized the model picker for CPU,
// labelled the backend `cpu`, and wasted the card in silence (#67).
//
// The engine we hand the model to (ollama) resolves GPUs by loading the
// driver library, so this detector is layered to agree with it:
//
//  1. nvidia-smi, resolved through a chain ($WAIRED_NVIDIA_SMI → $PATH →
//     the OS's well-known install locations) — richest output, and
//     authoritative in both directions when it runs.
//  2. the driver library itself (NVML on Windows) — ollama's own source
//     of truth, and reachable with no PATH and no CLI.
//  3. the OS's device inventory (display-class registry on Windows,
//     /proc + sysfs on Linux) — last resort, and gated on the driver
//     actually being installed so a removed card leaves no phantom.
//
// "No GPU" is returned only when every layer agrees. An NVIDIA adapter
// that no layer can enumerate is returned as an ERROR (surfaced through
// Profile.Errors), never as a silent CPU-only host.
func detectNvidia(ctx context.Context) ([]GPU, Accelerators, error) {
	smi := nvidiaSMIProbe(ctx)
	if smi.Ran {
		return smi.GPUs, Accelerators{CUDA: len(smi.GPUs) > 0}, nil
	}
	return classifyNvidia(smi, nvidiaFallback(ctx))
}

// classifyNvidia composes the nvidia-smi layer with the per-OS fallback
// into the detector's (gpus, accelerators, error) contract.
//
// It is a pure function so the four outcomes that matter — tool
// answered, tool silent but devices found, adapter present and
// unenumerable, host genuinely has no NVIDIA — are table-testable
// without a GPU, which is what no test could reach before (#67).
func classifyNvidia(smi nvidiaSMIResult, fb nvidiaFallbackResult) ([]GPU, Accelerators, error) {
	if smi.Ran {
		return smi.GPUs, Accelerators{CUDA: len(smi.GPUs) > 0}, nil
	}
	switch {
	case len(fb.GPUs) > 0:
		// Detected, with a soft warning: the same "data + warning"
		// contract detectAMD uses for registry-sourced adapters.
		return fb.GPUs, Accelerators{CUDA: true}, fmt.Errorf(
			"gpu(nvidia): %d device(s) detected via %s because nvidia-smi could not be used (%v)%s",
			len(fb.GPUs), fb.Source, smi.Err, nvidiaVRAMWarning(fb.GPUs))
	case fb.AdapterSeen:
		return nil, Accelerators{}, fmt.Errorf(
			"gpu(nvidia): an NVIDIA adapter is present (%s) but no device could be enumerated (%v); "+
				"install/repair the driver or point %s at nvidia-smi — inference runs on CPU until then",
			fb.Source, smi.Err, nvidiaSMIEnvOverride)
	case smi.Err != nil && !errors.Is(smi.Err, errNvidiaSMIAbsent):
		// nvidia-smi was found and failed. That is a real failure, and it
		// is not the same statement as "this host has no NVIDIA GPU".
		return nil, Accelerators{}, fmt.Errorf("gpu(nvidia): %w", smi.Err)
	default:
		// Nothing anywhere: a host with no NVIDIA GPU. The only branch
		// allowed to be quiet.
		return nil, Accelerators{}, nil
	}
}

// nvidiaVRAMWarning appends the AMD-style "VRAM unknown" note when a
// fallback source could not read capacity, so a 0 budget downstream is
// explained rather than mysterious.
func nvidiaVRAMWarning(gpus []GPU) string {
	missing := 0
	for _, g := range gpus {
		if g.VRAMTotalMB == 0 {
			missing++
		}
	}
	if missing == 0 {
		return ""
	}
	return fmt.Sprintf("; VRAM unknown for %d of %d device(s), so no VRAM budget is applied", missing, len(gpus))
}

// nvidiaSMIProbe resolves nvidia-smi and queries it.
func nvidiaSMIProbe(ctx context.Context) nvidiaSMIResult {
	path, ok := resolveNvidiaSMI(runtime.GOOS, os.Getenv, nvidiaSMIOnPATH())
	if !ok {
		return nvidiaSMIResult{Err: errNvidiaSMIAbsent}
	}
	gpus, err := queryNvidiaSMI(ctx, path)
	if err != nil {
		return nvidiaSMIResult{Err: err}
	}
	return nvidiaSMIResult{GPUs: gpus, Ran: true}
}

// nvidiaSMIOnPATH is step 2 of the resolution chain. PATH is a HINT
// here, never the verdict: the chain continues past an empty answer into
// the well-known install locations, and past those into the driver
// library itself. See detectNvidia and #67.
func nvidiaSMIOnPATH() string {
	p, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return ""
	}
	return p
}

// resolveNvidiaSMI returns the first candidate that exists on disk.
//
// Candidates may contain glob metacharacters (the Windows DriverStore
// entry does); filepath.Glob returns only paths that exist, so a literal
// candidate is filtered by the same call that expands a pattern.
func resolveNvidiaSMI(goos string, env func(string) string, onPATH string) (string, bool) {
	for _, cand := range nvidiaSMICandidates(goos, env, onPATH) {
		matches, err := filepath.Glob(cand)
		if err != nil {
			continue // malformed pattern; not fatal, try the next candidate
		}
		for _, m := range matches {
			if info, statErr := os.Stat(m); statErr == nil && !info.IsDir() {
				return m, true
			}
		}
	}
	return "", false
}

// nvidiaSMICandidates lists where to look for nvidia-smi on goos, in
// order. Pure: env and the $PATH hint are inputs, so all three OSes are
// table-testable from any host (CLAUDE.md §Cross-OS parity).
//
// Paths are built with literal separators rather than filepath.Join
// because the answer is about goos, not about the host running the code
// — the unit suite runs on all three (#261).
func nvidiaSMICandidates(goos string, env func(string) string, onPATH string) []string {
	var out []string
	if override := strings.TrimSpace(env(nvidiaSMIEnvOverride)); override != "" {
		out = append(out, override)
	}
	if onPATH != "" {
		out = append(out, onPATH)
	}
	switch goos {
	case "windows":
		// System32 is where modern drivers put it (and the one location a
		// LocalSystem service's PATH would normally cover); NVSMI is the
		// legacy location; DriverStore is where it always exists, under a
		// driver-version-stamped directory.
		if root := strings.TrimRight(env("SystemRoot"), `\`); root != "" {
			out = append(out,
				root+`\System32\nvidia-smi.exe`,
				root+`\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
			)
		}
		for _, key := range []string{"ProgramFiles", "ProgramW6432"} {
			if pf := strings.TrimRight(env(key), `\`); pf != "" {
				out = append(out, pf+`\NVIDIA Corporation\NVSMI\nvidia-smi.exe`)
			}
		}
	case "linux":
		out = append(out,
			"/usr/bin/nvidia-smi",
			"/usr/local/bin/nvidia-smi",
			"/opt/nvidia/bin/nvidia-smi",
			// WSL2 mounts the Windows driver's tooling here and puts it on
			// PATH only for login shells.
			"/usr/lib/wsl/lib/nvidia-smi",
		)
	}
	// darwin: no NVIDIA driver ships for Apple Silicon or current macOS,
	// so there is nothing to look for beyond an explicit override.
	return dedupeStrings(out)
}

// dedupeStrings drops repeats while preserving order — the $PATH hint
// usually resolves to a well-known location that is also listed below it.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// queryNvidiaSMI runs the resolved nvidia-smi and parses its CSV.
//
// A failure of the full query is retried once with the field set every
// shipping nvidia-smi understands: a driver that rejects one field exits
// non-zero, and the old code read that as "this host has no GPU".
func queryNvidiaSMI(ctx context.Context, path string) ([]GPU, error) {
	gpus, err := runNvidiaSMI(ctx, path, nvidiaSMIQueryFull, 5)
	if err == nil {
		return gpus, nil
	}
	basic, basicErr := runNvidiaSMI(ctx, path, nvidiaSMIQueryBasic, 3)
	if basicErr != nil {
		return nil, err // report the richer query's failure, not the retry's
	}
	return basic, nil
}

// runNvidiaSMI executes one CSV query and parses want columns from it.
func runNvidiaSMI(ctx context.Context, path, fields string, want int) ([]GPU, error) {
	cctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, path,
		"--query-gpu="+fields,
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi %s: %w", fields, err)
	}
	gpus, err := parseNvidiaSMICSV(string(out), want)
	if err != nil {
		return nil, fmt.Errorf("parse nvidia-smi: %w", err)
	}
	return gpus, nil
}

// parseNvidiaSMICSV parses `--query-gpu=` CSV output (with
// `--format=csv,noheader,nounits`) whose column count is want, in the
// order the query asked for: name, memory.total, driver_version, then
// compute_cap and uuid when present. memory.total is reported in MiB by
// `nounits` and stored verbatim as VRAMTotalMB.
//
// Whitespace around commas is tolerated (nvidia-smi inserts a single
// space after each comma in CSV output). Blank trailing lines are
// skipped.
func parseNvidiaSMICSV(s string, want int) ([]GPU, error) {
	var out []GPU
	for i, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != want {
			return nil, fmt.Errorf("line %d: expected %d CSV fields, got %d (%q)", i+1, want, len(fields), line)
		}
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		mb, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: memory.total = %q: %w", i+1, fields[1], err)
		}
		gpu := GPU{
			Vendor:        "nvidia",
			Model:         fields[0],
			VRAMTotalMB:   mb,
			DriverVersion: fields[2],
		}
		if want >= 5 {
			gpu.ComputeCap = fields[3]
			gpu.UUID = fields[4]
		}
		out = append(out, gpu)
	}
	return out, nil
}

// NVIDIADriverPresent reports whether this host has a usable NVIDIA GPU
// — the same question detectNvidia answers, exported for the pre-flight
// gates that used to ask "$PATH, is nvidia-smi there?" and got a wrong
// answer on service accounts (#67).
//
// Errors are intentionally dropped: a caller gating an expensive install
// wants a yes/no, and a partial detection (NVML / registry devices with
// unknown VRAM) still means the driver is there.
func NVIDIADriverPresent(ctx context.Context) bool {
	gpus, accel, _ := detectNvidia(ctx)
	return accel.CUDA || len(gpus) > 0
}
