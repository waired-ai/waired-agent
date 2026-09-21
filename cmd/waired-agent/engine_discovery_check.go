package main

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Checking the engine's own GPU discovery against waired's prediction, once
// the engine is up (waired-agent#1513). Report only: nothing here changes a
// decision.
//
// waired decides before ollama starts which GPUs ollama will use
// (hardware.partitionForEngine, a copy of ollama's own rules), and sizes
// models, picks downloads and fetches the ROCm overlay from that. A copy
// goes stale when an engine release changes the rules, and until now the
// only thing that would notice was a person re-reading upstream (#1486).
// The engine logs what it actually kept (runtime.ParseInferenceCompute), so
// the two can be compared at every start, and a disagreement says which
// side to re-read.

// enginePrediction is what the boot plan expected the engine to use, frozen
// with the plan: the profiler re-detects on its own TTL, and the check has
// to hold the engine to the prediction that drove the start.
type enginePrediction struct {
	gpus     []hardware.GPU
	setAside []hardware.UnusedGPU
	// igpuEnable is OLLAMA_IGPU_ENABLE as the engine was started with it:
	// the backend plan's value (the Windows Strix Halo arm), else the one
	// the daemon inherited. "" = unset.
	igpuEnable string
	// rocmOverlay is whether the overlay should be on disk.
	rocmOverlay bool
}

// engineVendor names the vendor of a device the engine reported: from the
// library where it says, from the description on Vulkan, which serves
// every vendor.
func engineVendor(d infruntime.EngineDevice) string {
	switch strings.ToLower(d.Library) {
	case "cuda":
		return "nvidia"
	case "rocm":
		return "amd"
	case "metal":
		return "apple"
	}
	desc := strings.ToLower(d.Description + " " + d.Name)
	switch {
	case strings.Contains(desc, "nvidia") || strings.Contains(desc, "geforce"):
		return "nvidia"
	case strings.Contains(desc, "amd") || strings.Contains(desc, "radeon"):
		return "amd"
	case strings.Contains(desc, "intel"):
		return "intel"
	}
	return "unknown"
}

// igpuAdmission is ollama's reading of OLLAMA_IGPU_ENABLE
// (envconfig.BoolWithDefault): unset leaves integrated GPUs to the
// default rule, a value that parses decides, and one that does not parse
// is read as true.
func igpuAdmission(v string) (explicit, allow bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return false, false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return true, b
	}
	return true, true
}

// compareEngineDiscovery lists how the engine's report differs from the
// prediction. Empty means they agree.
//
// Compared: how many devices of each vendor, CUDA compute capability and
// ROCm gfx target (as sets: the engine lists devices by free memory). Not
// compared: pci_id — waired keeps vendor:device pairs, the engine a bus
// address; a Vulkan or Metal compute of 0.0, which is no reading; and
// type=discrete, which the engine prints for any device it did not find
// integrated.
func compareEngineDiscovery(goos string, pred enginePrediction, got infruntime.EngineDiscovery) []string {
	type span struct{ min, max int }
	want := map[string]*span{}
	add := func(vendor string, definite bool) {
		v := strings.ToLower(vendor)
		if want[v] == nil {
			want[v] = &span{}
		}
		want[v].max++
		if definite {
			want[v].min++
		}
	}
	explicit, allow := igpuAdmission(pred.igpuEnable)
	for _, g := range pred.gpus {
		if explicit && !allow && goos != "darwin" && g.IntegratedKnown && g.Integrated {
			continue // the operator turned integrated GPUs off
		}
		add(g.Vendor, true)
	}
	for _, u := range pred.setAside {
		switch {
		case u.MemoryUnread:
			// Left out of the budget for want of a memory reading, not
			// because the engine skips it: it may well use it.
			add(u.Vendor, false)
		case explicit && allow:
			add(u.Vendor, true)
		}
	}
	have := map[string]int{}
	for _, d := range got.Devices {
		have[engineVendor(d)]++
	}

	var diffs []string
	vendors := map[string]bool{}
	for v := range want {
		vendors[v] = true
	}
	for v := range have {
		vendors[v] = true
	}
	names := make([]string, 0, len(vendors))
	for v := range vendors {
		names = append(names, v)
	}
	sort.Strings(names)
	for _, v := range names {
		w := want[v]
		if w == nil {
			w = &span{}
		}
		if n := have[v]; n < w.min || n > w.max {
			expect := strconv.Itoa(w.min)
			if w.max != w.min {
				expect = fmt.Sprintf("%d to %d", w.min, w.max)
			}
			diffs = append(diffs, fmt.Sprintf("the engine uses %d %s GPU(s); waired expected %s", n, v, expect))
		}
	}

	compare := func(library, what string, predicted func(hardware.GPU) string, vendor string) {
		var engine, ours []string
		for _, d := range got.Devices {
			if strings.EqualFold(d.Library, library) && d.Compute != "" && d.Compute != "0.0" {
				engine = append(engine, d.Compute)
			}
		}
		for _, g := range pred.gpus {
			if strings.EqualFold(g.Vendor, vendor) {
				if c := predicted(g); c != "" {
					ours = append(ours, c)
				}
			}
		}
		if len(engine) == 0 || len(ours) == 0 || len(engine) != len(ours) {
			return
		}
		sort.Strings(engine)
		sort.Strings(ours)
		if strings.Join(engine, ",") != strings.Join(ours, ",") {
			diffs = append(diffs, fmt.Sprintf("%s %s is %s on the engine; waired read %s",
				library, what, strings.Join(engine, ","), strings.Join(ours, ",")))
		}
	}
	compare("CUDA", "compute capability", func(g hardware.GPU) string { return g.ComputeCap }, "nvidia")
	compare("ROCm", "target", func(g hardware.GPU) string { return g.GFXTarget }, "amd")

	if pred.rocmOverlay && have["amd"] > 0 {
		rocm := false
		for _, d := range got.Devices {
			if strings.EqualFold(d.Library, "ROCm") {
				rocm = true
			}
		}
		if !rocm {
			diffs = append(diffs, "no AMD device came up on ROCm although the ROCm overlay should be installed; it may be missing")
		}
	}
	return diffs
}

// igpuEnableFor is OLLAMA_IGPU_ENABLE as the engine is started with it: the
// backend plan's env wins over the inherited one, as it does in the
// adapter's process env.
func igpuEnableFor(planEnv []string, getenv func(string) string) string {
	const key = "OLLAMA_IGPU_ENABLE="
	for _, kv := range planEnv {
		if strings.HasPrefix(kv, key) {
			return strings.TrimPrefix(kv, key)
		}
	}
	return getenv("OLLAMA_IGPU_ENABLE")
}

// describeEngineDevices renders the engine's devices for the log line.
func describeEngineDevices(ds []infruntime.EngineDevice) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		desc := strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, d.Description)
		if len(desc) > 80 {
			desc = desc[:80]
		}
		out = append(out, fmt.Sprintf("%s %s %s %s", d.Library, d.Compute, d.Type, desc))
	}
	return out
}

// reportEngineDiscovery reads the engine's device block and logs whether
// it matches the prediction: INFO when it does, WARN when it does not. Once
// per process, after the engine answers. An adopted engine is skipped: its
// output is not in this agent's engine.log.
func (p *agentInferenceProvider) reportEngineDiscovery() {
	if p.ollama == nil || p.ollama.Mode() == infruntime.EngineModeAdopted {
		return
	}
	head := p.ollama.EngineDiscoveryHead()
	if head == "" {
		return
	}
	got, ok := infruntime.ParseInferenceCompute(head)
	if !ok {
		p.logger.Info("engine GPU discovery not read from the engine log; not compared with waired's prediction")
		return
	}
	pred := p.bootPlan.prediction
	predicted := make([]string, 0, len(pred.gpus))
	for _, g := range pred.gpus {
		predicted = append(predicted, g.Vendor+" "+g.Model)
	}
	setAside := make([]string, 0, len(pred.setAside))
	for _, u := range pred.setAside {
		setAside = append(setAside, u.Vendor+" "+u.Model)
	}
	engine := describeEngineDevices(got.Devices)
	if got.CPUOnly {
		engine = []string{"cpu"}
	}
	if diffs := compareEngineDiscovery(runtime.GOOS, pred, got); len(diffs) > 0 {
		p.logger.Warn("the engine's GPU discovery differs from waired's prediction",
			"differences", diffs, "engine", engine, "engine_dropped", describeEngineDevices(got.Dropped),
			"predicted", predicted, "set_aside", setAside, "igpu_enable", pred.igpuEnable)
		return
	}
	p.logger.Info("the engine's GPU discovery matches waired's prediction",
		"engine", engine, "set_aside", setAside)
}
