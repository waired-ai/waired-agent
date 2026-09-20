package hardware

import "strings"

// NVIDIA's single-pool parts, and the one thing that identifies them.
//
// WHY A TABLE HERE AND NOT A READING. The other two vendors are answered
// by a fact the machine reports: Apple Silicon by its architecture,
// Linux AMD by the amdgpu FUSION bit, Windows by the arithmetic in
// carvedFromSystemRAM. NVIDIA has neither on the platforms this binary
// can reach — CGO_ENABLED=0 rules out cudaDeviceProp.integrated, NVML
// carries no equivalent field, and `nvidia-smi` exposes no column for it.
// So the part is named, from what the vendor published about it.
//
// WHY THE DEVICE NAME IS THE KEY, having ruled out the alternatives:
//
//   - CPU.Model is EMPTY on these machines. aarch64 Linux has no "model
//     name" line in /proc/cpuinfo (ARM has no CPUID equivalent and the
//     upstream maintainers have repeatedly declined to add one), and
//     defaultCPU reads only that line. Grace, GB10 and Jetson are all
//     aarch64. So the key #251 uses for Apple and AMD cannot work here.
//   - ComputeCap is NOT UNIQUE. Compute capability 12.1 covers both the
//     GB10 (DGX Spark, 128 GB at 273 GB/s) and the RTX Spark N1X
//     (Windows on Arm, a ~45 GiB CUDA pool) — two parts that differ in
//     both capacity and bandwidth. Keying on it would recreate exactly
//     the defect waired-agent#1455 removed, where an L4 and an RTX PRO
//     4000 Blackwell shared the name "nvidia-24gb-discrete" and differed
//     2.24x on bandwidth.
//   - The PCI pair DOES NOT EXIST for this part. pci.ids carries no GPU
//     device ID for GB10 at all — only the PCIe host bridges 22ce
//     ("GB10 GEN5 X4 PCIe host") and 22d0. The N1X reportedly ships more
//     than one device ID.
//
// ON "do not parse". GPU.Model's doc says free-form, do not parse, and
// waired-agent#1455's decision record repeated it. That rule binds
// CONSUMERS of the published hardware summary, and this is the producer
// side turning a string into the facts it then publishes — the same
// carve-out #251 relied on to key the Apple bandwidth table off
// CPU.Model, stated in that issue as "the producer side turning a string
// into the number it then publishes, which is what keeps consumers
// honest". What this package publishes is UnifiedMemory, a boolean, so
// no consumer re-derives any of this from prose.
//
// The string itself comes from NVIDIA's own library on both platforms —
// `nvidia-smi --query-gpu=name` on Linux, nvmlDeviceGetName on Windows —
// so unlike the AMD CPU string there is no cross-OS spelling split to
// normalise away.
//
// FAILURE DIRECTION. A part NVIDIA renames falls out of this table and
// reads "unknown", which is today's behaviour and costs nothing that is
// not already being paid. That is the safe direction, and it is why the
// name is preferable to compute capability, whose failure direction is
// the dangerous one: a future DISCRETE part sharing a capability with a
// single-pool one would be credited with a pool it does not have.

// nvidiaUnifiedPart is what this package knows about one single-pool
// NVIDIA part: how to name it in a provenance key, and how fast its pool
// is read.
type nvidiaUnifiedPart struct {
	// slug names the part in a derived host key, so that the key is
	// unified-nvidia-gb10 rather than unified-nvidia-sm121. See the
	// ComputeCap note above for why the capability is not enough: two
	// parts share 12.1, and a provenance key whose job is to say which
	// machine took a measurement must not fold them together.
	slug string

	// bandwidthGBs is the vendor's published PEAK memory bandwidth in
	// GB/s, on the same terms as appleUnifiedBandwidthGBs: an upper
	// bound, never a measurement, never extrapolated (#273). 0 means the
	// figure was not published, which is a normal answer — hostfit falls
	// back to its population constant and stays annotate-only.
	bandwidthGBs float64
}

// nvidiaUnifiedParts maps the device name NVIDIA's own tooling reports,
// normalised, to what is known about that part.
//
// KEYS ARE MATCHED EXACTLY AFTER NORMALISATION, NEVER BY PREFIX OR
// SUBSTRING, and here that is not a style preference — a substring match
// is wrong on real, shipping hardware. In pci.ids "GB10" is a prefix of
// three other codenames:
//
//	2901  GB100 [B200]          -- discrete, HBM3e
//	29bc  GB102 [B100]          -- discrete, HBM3e
//	2b00  GB10B [Jetson AGX Thor]
//
// The first two are discrete data-centre parts. A prefix table would
// credit a B200 with one pool. Exact match has no such hazard, and a
// string that does not match falls through to "unknown", which is the
// safe answer anyway. appleUnifiedBandwidthGBs carries the same rule for
// the same reason ("Apple M4 Max" containing "Apple M4").
var nvidiaUnifiedParts = map[string]nvidiaUnifiedPart{
	// DGX Spark. 128 GB LPDDR5x described by NVIDIA as "coherent unified
	// system memory", 273 GB/s over a 256-bit bus (NVIDIA's DGX Spark
	// product specifications). The pool the driver offers is ~121.7 GiB,
	// which is also what /proc/meminfo reports as MemTotal — one pool,
	// two readings, which is the question integrated.go asks.
	//
	// `nvidia-smi --query-gpu=name,memory.total,compute_cap` answers
	// exactly "NVIDIA GB10, [N/A], 12.1" here. NVIDIA documents the
	// [N/A]: "On iGPU platforms, nvidia-smi will display 'Memory-Usage:
	// Not Supported' … because iGPUs do not have dedicated framebuffer
	// memory" (DGX Spark known issues).
	"nvidia gb10": {slug: "gb10", bandwidthGBs: 273.0},

	// DELIBERATELY ABSENT, each for a different reason:
	//
	// GH200 and the Grace-Hopper superchips. Coherent over NVLink-C2C
	// but NOT one pool: Grace has its own LPDDR5X and Hopper its own
	// HBM3, and CUDA reports integrated == 0. integrated.go's header
	// covers why coherence is a different axis from topology.
	//
	// RTX Spark N1X. It IS a single-pool part, but no report gives the
	// device name nvidia-smi prints for it, and no peak bandwidth has
	// been published. Guessing either would put a wrong answer in the
	// one place this package may refuse a model (#273). It does not need
	// an entry on the platform it ships on: Windows detects it through
	// carvedFromSystemRAM without consulting any name, because its
	// carve-out (~7.9 GiB against ~9.8 GiB of firmware deduction) is
	// exactly the shape that arithmetic was written for.
	//
	// Jetson (Orin, Thor). Single-pool, but nothing in this repository
	// has run on one, the name `nvidia-smi` prints there is reported
	// inconsistently, and several Jetson boards ship no nvidia-smi at
	// all. An absent part keeps today's behaviour.
}

// nvidiaUnifiedPartFor looks up a device name in the table above.
func nvidiaUnifiedPartFor(model string) (nvidiaUnifiedPart, bool) {
	p, ok := nvidiaUnifiedParts[normalizeChipName(model)]
	return p, ok
}

// integratedFromVendor is the vendor axis of the question integrated.go
// asks: what can be said about this device from what the VENDOR
// published about the part, independent of the operating system.
//
// It exists beside integratedFromOS rather than inside it because the
// answer is the same on every platform — the part is one pool wherever
// it is plugged in — and a copy in each build-tagged file is how three
// answers to one question drift apart (CLAUDE.md §Cross-OS parity).
// Untagged also means it is reachable from a test on any host, which
// matters more than usual here: this repository has no NVIDIA
// single-pool machine to run against.
//
// WHAT IT DELIBERATELY DOES NOT DO: it does not read the memory figure.
// A tempting second rule is "the device reports a total close to system
// RAM, therefore one pool" — nvitop proposed exactly that, at 90 %. It
// is unsound: a 32 GB desktop with a 32 GB RTX 5090, or a 24 GB one with
// a 24 GB RTX 4090, satisfies it while having two entirely separate
// pools. The inverse rule, "the device reports NO total, therefore one
// pool", is unsound in the other direction: MIG parent devices and some
// vGPU guests also decline to answer, and the sentinel this repo already
// parses covers "[Insufficient Permissions]" too, so a permissions
// problem would read as a hardware topology. Both were considered and
// rejected; the name is the evidence.
func integratedFromVendor(prof *Profile, i int) integration {
	if prof == nil || i < 0 || i >= len(prof.GPUs) {
		return integrationUnknown()
	}
	g := prof.GPUs[i]
	if !strings.EqualFold(g.Vendor, "nvidia") {
		return integrationUnknown()
	}
	if _, ok := nvidiaUnifiedPartFor(g.Model); ok {
		return integratedKnown(true)
	}
	return integrationUnknown()
}

// nvidiaUnifiedHost reports whether this profile's primary accelerator
// is an NVIDIA part that was DETECTED as one pool — by the table above,
// or by any other source the merge accepted, which on Windows means
// carvedFromSystemRAM answering for a part no table names.
//
// It reads GPUs[0] because that is the device every other budget
// decision in this package reads (EffectiveVRAMMB, PrimaryGPUVendor,
// HostTopologyOf). That entry is enumeration order rather than a
// ranking, which is waired-agent#286 and is not fixed here.
func nvidiaUnifiedHost(p *Profile) bool {
	if p == nil || len(p.GPUs) == 0 {
		return false
	}
	g := p.GPUs[0]
	return g.IntegratedKnown && g.Integrated && strings.EqualFold(g.Vendor, "nvidia")
}
