package hardware

// The question this package asks about an accelerator, written once
// because three operating systems answer it three different ways and the
// QUESTION is the part that has to stay the same (waired-agent#459).
//
// THE QUESTION: are the accelerator's memory and the operating system's
// RAM two readings of ONE physical pool?
//
// THE NAME: an accelerator for which the answer is yes is an INTEGRATED
// GPU. That is not a term coined here. ggml's enum spells it
// GGML_BACKEND_DEVICE_TYPE_IGPU and documents it as "integrated GPU
// device using host memory"; CUDA's cudaDeviceProp.integrated is "Device
// is integrated as opposed to discrete"; ollama carries
// DeviceInfo.Integrated, vLLM is_integrated_gpu(), PyTorch is_integrated,
// Vulkan VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU, Level Zero
// ZE_DEVICE_PROPERTY_FLAG_INTEGRATED, HIP hipDeviceAttributeIntegrated.
// Six independent implementations settled on one word.
//
// WHAT THIS IS NOT: it is not a question about cache coherence, and the
// two must not be conflated. NVIDIA's GH200 reports integrated == 0 and
// is fully coherent — Grace has its own LPDDR5X and Hopper its own HBM3,
// coherent over NVLink-C2C but two pools. GB10 (DGX Spark) and Jetson
// report integrated == 1 and are one pool. The coherence axis has its
// own vendor names (ATS and HMM, reported by `nvidia-smi -q` as
// "Addressing Mode") and answers a different question: whether the GPU
// can dereference a malloc pointer, not whether counting its memory and
// counting RAM counts the same bytes twice. This package wants the
// second one, because that is what the capacity gate and the decode
// estimate get wrong on a single-pool part.
//
// WHY THE ANSWER IS THREE-STATE: none of the shell-reachable facts is
// available everywhere, and the failure mode of a missing fact is
// SILENCE, not a negative. A GB10 whose Vulkan loader is missing
// libnvidia-gl reports no device at all rather than a discrete one; a
// Linux AMD host whose render node cannot be opened yields no reading
// rather than "not integrated". Reading either silence as "discrete" is
// how a single-pool part inherits the discrete assumptions, which is the
// defect waired-agent#459 was opened for. So "unknown" is a distinct
// answer and it is the safe default: it leaves today's behaviour in
// place. ollama reached the same shape independently
// (discover/native_probe.go's Integrated + IntegratedKnown pair).

// integration is one source's answer about one device.
//
// The zero value is the honest starting point: not integrated, and not
// known to be either way.
type integration struct {
	// integrated is the answer, meaningful only where known is true —
	// except through merge below, which lets an unknown source that saw
	// evidence OF integration still be heard.
	integrated bool
	// known records that a source actually answered, as opposed to
	// finding nothing. A false/false pair means "no source spoke".
	known bool
}

// integratedKnown is the answer of a source that is certain.
func integratedKnown(yes bool) integration { return integration{integrated: yes, known: true} }

// integrationUnknown is the answer of a source that could not tell —
// the tool was absent, the device node would not open, the field was not
// present. It is NOT the answer "discrete".
func integrationUnknown() integration { return integration{} }

// merge folds another source's answer into this one, asymmetrically.
//
// The asymmetry is the whole point, and it follows from which direction
// each error hurts. Crediting a discrete host with one pool would make
// the capacity gate count the card's VRAM as if it were RAM — a claim
// about memory the host does not have. Crediting a single-pool host with
// two is the defect #459 describes, but it is also EXACTLY today's
// behaviour, so it costs nothing that is not already being paid. One of
// those is a regression and the other is the status quo, so:
//
//   - a source that KNOWS overrides whatever came before, either way;
//   - a source that does not know may still promote "not integrated" to
//     "integrated", because seeing evidence of one pool is a positive
//     observation even from a source that cannot rule the opposite out;
//   - a source that does not know can never demote, and can never turn
//     a known answer into an unknown one.
//
// ollama's probe merge (discover/native_probe.go) is the same rule, and
// the reason is the same there: its probes disagree about coverage, not
// about the hardware.
func (a integration) merge(b integration) integration {
	switch {
	case b.known:
		return b
	case b.integrated:
		return integration{integrated: true, known: a.known}
	default:
		return a
	}
}

// carvedFromSystemRAM answers the question for an adapter whose memory
// figure can be compared against what the firmware took away from RAM.
//
// It lives here, untagged, rather than beside the Windows API calls that
// feed it, because the arithmetic is the part worth testing and a
// Windows build tag would put it out of reach of every machine but one.
// Same reason strixHaloUMA takes a goos rather than living in two
// build-tagged copies. The measured cases — this repo's reference host
// in both of its carve-out configurations, and a discrete card — are in
// TestCarvedFromSystemRAM.
//
// It is ONE-SIDED on purpose: see the "why it only ever says yes" note
// in integrated_windows.go. A figure larger than the deduction is not
// evidence of a discrete part, only evidence that this arithmetic does
// not apply to it.
func carvedFromSystemRAM(adapterBytes, installedBytes, visibleBytes uint64) integration {
	if adapterBytes == 0 || installedBytes == 0 || visibleBytes == 0 {
		return integrationUnknown()
	}
	if installedBytes < visibleBytes {
		// Malformed SMBIOS. GetPhysicallyInstalledSystemMemory
		// documents this as its own failure (ERROR_INVALID_DATA); if it
		// ever reaches here the difference is meaningless.
		return integrationUnknown()
	}
	if adapterBytes <= installedBytes-visibleBytes {
		return integratedKnown(true)
	}
	return integrationUnknown()
}

// integratedFrom is the shape every per-OS answer takes: it is handed
// the profile as the detectors left it and reports what this platform
// can say about the device at index i of prof.GPUs.
//
// Declared here rather than in each build-tagged file so the three
// implementations cannot drift into three different signatures, which is
// how "the same question" quietly becomes three questions.
type integratedFrom func(prof *Profile, i int) integration
