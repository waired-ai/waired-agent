//go:build linux

package hardware

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Linux's answer to the question in integrated.go, for AMD.
//
// THE FACT: the amdgpu driver publishes, through DRM_IOCTL_AMDGPU_INFO,
// an ids_flags word whose bit 0 the UAPI header names
// AMDGPU_IDS_FLAGS_FUSION and comments "Flag that this is integrated
// (a.k.a. fusion) GPU". The kernel sets it from AMD_IS_APU
// (amdgpu_kms.c). Mesa reads exactly this one bit and calls the result
// what we want to call it too:
//
//	info->has_dedicated_vram = !(device_info->ids_flags & AMDGPU_IDS_FLAGS_FUSION);
//	                                                  -- src/amd/common/ac_gpu_info.c
//
// WHY NOT SYSFS. Everything world-readable was measured on this repo's
// own Linux host (a Granite Ridge iGPU beside a discrete NVIDIA card,
// 2026-09-20) and none of it answers:
//
//   - KFD topology's cpu_cores_count, which ROCr derives its
//     HSA_AMD_MEMORY_PROPERTY_AGENT_IS_APU from, reads 0 on that APU.
//     The GPU sits in its own node. AMD's own ROCm/rocm-systems#8190
//     records the same unreliability from the other end.
//   - local_mem_size is hardcoded to 0 for every device in
//     kfd_topology.c, unchanged from v5.10 to master.
//   - The iGPU advertises max_link_width 16 at 16 GT/s, the same PCIe
//     link a discrete card advertises.
//   - mem_info_vram_total is the firmware carve-out, which is 2 GiB on
//     that host and 96 GiB on the Strix Halo reference host. Any
//     threshold on it classifies one of the two wrongly, which is the
//     bug ollama shipped (discover/amd.go's vram <= 4 GiB rule) and
//     LM Studio is still carrying (lmstudio-ai/lms#589).
//
// AMD acknowledges the gap: ROCm/rocm-systems#8476, "APUs are not
// identifiable through amdsmi", open.
//
// WHO CAN READ IT. The render node is mode 0660 root:render. The daemon
// runs as User=waired with no supplementary groups, so it cannot open
// it, and granting it the group standing would buy a privilege for the
// sake of a fact that never changes. Instead `sudo waired init` — which
// is already the elevated path, and already writes state that
// service_linux.go's FixStateOwnership chowns back — takes the reading
// once and persists it, the way host-memory.json persists the
// available-memory measurement. This function is still called on every
// profile: where the node does happen to open it is the better source,
// and where it does not the answer is UNKNOWN, never "discrete".

// Constants transcribed from include/uapi/drm/amdgpu_drm.h.
const (
	// amdgpuInfoDevInfo is AMDGPU_INFO_DEV_INFO.
	amdgpuInfoDevInfo = 0x16
	// amdgpuIDsFlagsFusion is AMDGPU_IDS_FLAGS_FUSION, bit 0 of
	// drm_amdgpu_info_device.ids_flags.
	amdgpuIDsFlagsFusion = 0x01
	// amdPCIVendorID is the vendor word /sys/class/drm/*/device/vendor
	// reports for an AMD device.
	amdPCIVendorID = "0x1002"
)

// drmIoctlAmdgpuInfo is DRM_IOCTL_AMDGPU_INFO, which the header defines
// as DRM_IOW(DRM_COMMAND_BASE + DRM_AMDGPU_INFO, struct drm_amdgpu_info).
//
// Spelled as the _IOC arithmetic rather than as a magic number so the
// derivation is checkable against the header without a calculator:
// direction _IOC_WRITE (1) in the top 2 bits, then the argument size in
// 14 bits, then the type letter 'd', then the command number
// DRM_COMMAND_BASE (0x40) + DRM_AMDGPU_INFO (0x05).
const drmIoctlAmdgpuInfo = (1 << 30) |
	(uintptr(unsafe.Sizeof(amdgpuInfoRequest{})) << 16) |
	('d' << 8) |
	(0x40 + 0x05)

// amdgpuInfoRequest is struct drm_amdgpu_info. The trailing union is
// carried as opaque bytes because AMDGPU_INFO_DEV_INFO reads none of it,
// and naming members we never set would invite someone to set them.
type amdgpuInfoRequest struct {
	returnPointer uint64
	returnSize    uint32
	query         uint32
	_             [16]byte
}

// amdgpuInfoDevice is the prefix of struct drm_amdgpu_info_device up to
// and including the field this file wants. The kernel writes
// min(returnSize, sizeof(its own struct)) bytes, so asking for only the
// prefix is safe and keeps the transcription short enough to audit.
//
// TestAMDGPUInfoDeviceLayout pins idsFlags at the offset the header
// puts it; a field added or reordered above it moves that offset and
// fails there rather than silently reading a neighbour.
type amdgpuInfoDevice struct {
	deviceID                 uint32
	chipRev                  uint32
	externalRev              uint32
	pciRev                   uint32
	family                   uint32
	numShaderEngines         uint32
	numShaderArraysPerEngine uint32
	gpuCounterFreq           uint32
	maxEngineClock           uint64
	maxMemoryClock           uint64
	cuActiveNumber           uint32
	cuAOMask                 uint32
	cuBitmap                 [16]uint32
	enabledRBPipesMask       uint32
	numRBPipes               uint32
	numHWGfxContexts         uint32
	pcieGen                  uint32
	idsFlags                 uint64
}

// amdgpuFusionFlag asks one render node whether its device is an
// integrated (fusion) part. It reports ok=false for every reason that is
// not an answer — the node will not open, the ioctl is refused, the
// driver is not amdgpu — so the caller can tell "no" from "did not say".
func amdgpuFusionFlag(node string) (fusion, ok bool) {
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		// Overwhelmingly EACCES: mode 0660 root:render and we are not
		// in the group. Not an answer.
		return false, false
	}
	defer f.Close() //nolint:errcheck // read-only query; a close error changes nothing

	var dev amdgpuInfoDevice
	req := amdgpuInfoRequest{
		returnPointer: uint64(uintptr(unsafe.Pointer(&dev))),
		returnSize:    uint32(unsafe.Sizeof(dev)),
		query:         amdgpuInfoDevInfo,
	}
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		drmIoctlAmdgpuInfo,
		uintptr(unsafe.Pointer(&req)),
	); errno != 0 {
		// ENOTTY on a non-amdgpu node, EINVAL on a kernel that does not
		// know the query. Either way we learned nothing.
		return false, false
	}
	return dev.idsFlags&amdgpuIDsFlagsFusion != 0, true
}

// amdRenderNodes lists the render nodes whose PCI vendor is AMD.
//
// Vendor is read from sysfs rather than inferred from the node number:
// renderD128 is the first DRM device the kernel happened to bind, which
// on this repo's own Linux host is the NVIDIA card, with the AMD iGPU at
// renderD129. Nothing orders them.
func amdRenderNodes() []string {
	entries, err := filepath.Glob("/sys/class/drm/renderD*")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := filepath.Base(e)
		b, err := os.ReadFile(filepath.Join(e, "device", "vendor"))
		if err != nil || strings.TrimSpace(string(b)) != amdPCIVendorID {
			continue
		}
		out = append(out, filepath.Join("/dev/dri", name))
	}
	return out
}

// integratedFromOS is Linux's answer for prof.GPUs[i].
//
// Only AMD is answered here. NVIDIA on Linux has no equivalent reading
// without linking CUDA, which CGO_ENABLED=0 forecloses, and NVML — the
// one library the Windows side does load dynamically — carries no
// integrated flag at all; on a GB10 its memory query is refused
// outright. That path stays unknown until something can answer it, which
// is the status quo and is what waired-agent#459 asks NOT be mistaken
// for "discrete".
func integratedFromOS(prof *Profile, i int) integration {
	if i < 0 || i >= len(prof.GPUs) || !strings.EqualFold(prof.GPUs[i].Vendor, "amd") {
		return integrationUnknown()
	}
	nodes := amdRenderNodes()
	if len(nodes) != 1 {
		// Zero: no AMD device in sysfs, so nothing to ask. More than
		// one: the profile's GPU entries carry no PCI address (they come
		// from rocm-smi's row order), so there is no honest way to say
		// WHICH node is this entry. Guessing would answer for the wrong
		// device, and a wrong "discrete" here is the regression the
		// merge rule exists to prevent.
		return integrationUnknown()
	}
	fusion, ok := amdgpuFusionFlag(nodes[0])
	if !ok {
		return integrationUnknown()
	}
	return integratedKnown(fusion)
}
