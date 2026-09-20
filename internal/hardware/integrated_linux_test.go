//go:build linux

package hardware

import (
	"testing"
	"unsafe"
)

// The two structs in integrated_linux.go are transcriptions of C types
// in include/uapi/drm/amdgpu_drm.h, and a transcription is only as good
// as the thing that checks it. A field added or retyped above idsFlags
// moves it, and the ioctl would then return a neighbouring field's bits
// — which would still be a plausible-looking 0 or 1.
//
// The expected numbers are derived by hand from the header, so that a
// disagreement points at which side moved:
//
//	struct drm_amdgpu_info_device, up to ids_flags
//	  8 x __u32                    ->   0..32
//	  2 x __u64                    ->  32..48
//	  2 x __u32                    ->  48..56
//	  __u32 cu_bitmap[4][4]        ->  56..120
//	  4 x __u32                    -> 120..136
//	  __u64 ids_flags              -> 136
//
//	struct drm_amdgpu_info
//	  __u64 return_pointer         ->   0
//	  __u32 return_size, query     ->   8, 12
//	  union (largest member is 4 x __u32) -> 16..32
//
// This is a record of the header as read on 2026-09-20 at
// torvalds/linux master, not a product contract: the kernel may extend
// these structs, and this test is how that arrives as a failure instead
// of as a wrong answer.
func TestAMDGPUInfoDeviceLayout(t *testing.T) {
	if got := unsafe.Offsetof(amdgpuInfoDevice{}.idsFlags); got != 136 {
		t.Errorf("offsetof(drm_amdgpu_info_device.ids_flags) = %d, want 136", got)
	}
	if got := unsafe.Sizeof(amdgpuInfoRequest{}); got != 32 {
		t.Errorf("sizeof(drm_amdgpu_info) = %d, want 32", got)
	}
}

// DRM_IOCTL_AMDGPU_INFO is DRM_IOW(DRM_COMMAND_BASE + DRM_AMDGPU_INFO,
// struct drm_amdgpu_info). The constant is written as the _IOC
// arithmetic so it stays checkable, and this pins the value that
// arithmetic produces — if sizeof(drm_amdgpu_info) ever changes, the
// request number changes with it and the kernel answers ENOTTY, which
// integratedFromOS would report as "unknown" forever and silently.
func TestDRMIoctlAmdgpuInfoNumber(t *testing.T) {
	const want = 0x40206445
	if drmIoctlAmdgpuInfo != want {
		t.Errorf("DRM_IOCTL_AMDGPU_INFO = %#x, want %#x "+
			"(_IOC_WRITE | 32<<16 | 'd'<<8 | 0x45)", drmIoctlAmdgpuInfo, want)
	}
}

// A device the profile does not call AMD is not asked, whatever else is
// on the machine. The fleet's own Linux host is the case that makes this
// matter: it carries a discrete NVIDIA card at renderD128 and an AMD
// iGPU at renderD129, and asking the amdgpu ioctl about the NVIDIA entry
// would answer for the wrong device.
func TestIntegratedFromOS_SkipsNonAMD(t *testing.T) {
	prof := &Profile{GPUs: []GPU{
		{Vendor: "nvidia", Model: "a discrete card"},
		{Vendor: "intel", Model: "an iGPU we have no reading for"},
	}}
	for i := range prof.GPUs {
		if got := integratedFromOS(prof, i); got != integrationUnknown() {
			t.Errorf("GPUs[%d] (%s) = %+v, want unknown",
				i, prof.GPUs[i].Vendor, got)
		}
	}
}

// Out-of-range indices are unknown rather than a panic: the loop that
// calls this walks prof.GPUs, but the signature does not promise that.
func TestIntegratedFromOS_IndexOutOfRange(t *testing.T) {
	prof := &Profile{GPUs: []GPU{{Vendor: "amd"}}}
	for _, i := range []int{-1, 1, 99} {
		if got := integratedFromOS(prof, i); got != integrationUnknown() {
			t.Errorf("index %d = %+v, want unknown", i, got)
		}
	}
}
