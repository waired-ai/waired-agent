//go:build linux

package hardware

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// TestIntelQueryLayouts pins every size and ioctl number the Intel
// memory query depends on, against include/uapi/drm/xe_drm.h and
// i915_drm.h at Linux v7.0. No Intel discrete card has been available to
// run the query against (waired-agent#1483), so the transcription is the
// thing to check: a field added or reordered here fails in this test
// instead of reading a neighbour on a real card.
func TestIntelQueryLayouts(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uintptr
	}{
		{"struct drm_xe_device_query", unsafe.Sizeof(xeDeviceQuery{}), 40},
		{"struct drm_xe_mem_region", unsafe.Sizeof(xeMemRegion{}), 88},
		{"drm_xe_mem_region.total_size offset", unsafe.Offsetof(xeMemRegion{}.totalSize), 8},
		{"struct drm_i915_query", unsafe.Sizeof(i915Query{}), 16},
		{"struct drm_i915_query_item", unsafe.Sizeof(i915QueryItem{}), 24},
		{"struct drm_i915_memory_region_info", unsafe.Sizeof(i915MemoryRegionInfo{}), 88},
		{"drm_i915_memory_region_info.probed_size offset", unsafe.Offsetof(i915MemoryRegionInfo{}.probedSize), 8},
		// DRM_IOWR(DRM_COMMAND_BASE + 0x00, 40 bytes) and
		// DRM_IOWR(DRM_COMMAND_BASE + 0x39, 16 bytes).
		{"DRM_IOCTL_XE_DEVICE_QUERY", drmIOWR(drmXEDeviceQuery, unsafe.Sizeof(xeDeviceQuery{})), 0xC0286440},
		{"DRM_IOCTL_I915_QUERY", drmIOWR(drmI915Query, unsafe.Sizeof(i915Query{})), 0xC0106479},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

// The answer buffers, built the way each driver lays them out: a system
// memory region that must not be counted, and the card's VRAM.
func TestIntelQueryParsers(t *testing.T) {
	const vram = uint64(12) << 30
	t.Run("xe", func(t *testing.T) {
		buf := make([]byte, 8+2*88)
		binary.LittleEndian.PutUint32(buf[0:], 2)
		sys := buf[8:]
		binary.LittleEndian.PutUint16(sys[0:], 0) // DRM_XE_MEM_REGION_CLASS_SYSMEM
		binary.LittleEndian.PutUint64(sys[8:], 64<<30)
		dev := buf[8+88:]
		binary.LittleEndian.PutUint16(dev[0:], 1) // DRM_XE_MEM_REGION_CLASS_VRAM
		binary.LittleEndian.PutUint64(dev[8:], vram)
		if got := sumXEVRAM(buf); got != vram {
			t.Errorf("sumXEVRAM = %d, want %d", got, vram)
		}
	})
	t.Run("i915", func(t *testing.T) {
		buf := make([]byte, 16+2*88)
		binary.LittleEndian.PutUint32(buf[0:], 2)
		sys := buf[16:]
		binary.LittleEndian.PutUint16(sys[0:], 0) // I915_MEMORY_CLASS_SYSTEM
		binary.LittleEndian.PutUint64(sys[8:], 64<<30)
		dev := buf[16+88:]
		binary.LittleEndian.PutUint16(dev[0:], 1) // I915_MEMORY_CLASS_DEVICE
		binary.LittleEndian.PutUint64(dev[8:], vram)
		if got := sumI915DeviceMemory(buf); got != vram {
			t.Errorf("sumI915DeviceMemory = %d, want %d", got, vram)
		}
	})
	t.Run("a count larger than the buffer stops at the buffer", func(t *testing.T) {
		buf := make([]byte, 8+88)
		binary.LittleEndian.PutUint32(buf[0:], 5)
		binary.LittleEndian.PutUint16(buf[8:], 1)
		binary.LittleEndian.PutUint64(buf[16:], vram)
		if got := sumXEVRAM(buf); got != vram {
			t.Errorf("sumXEVRAM = %d, want %d", got, vram)
		}
	})
}
