//go:build linux

package hardware

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Reading an Intel discrete card's memory size on Linux
// (waired-agent#1483).
//
// Neither driver publishes it in sysfs — xe has only a d3cold threshold
// there and i915 nothing — so it comes from each driver's own query
// ioctl on the render node: xe's DRM_XE_DEVICE_QUERY_MEM_REGIONS and
// i915's DRM_I915_QUERY_MEMORY_REGIONS, the same two queries Mesa's
// anv/iris use to size local memory. Both are DRM_RENDER_ALLOW, so the
// only obstacle is the node's mode (0660 root:render on Debian-family
// systems). The installer puts the service user in `render` (#1535), so
// the daemon reads it itself. `sudo waired init` also takes it and
// persists it alongside the integration reading
// (cmd/waired/init_gpu_topology.go), the floor for a host whose service
// user is not in the group.
//
// No Intel discrete card has been available to measure this against. The
// layouts below are transcribed from include/uapi/drm/xe_drm.h and
// i915_drm.h at Linux v7.0, and TestIntelQueryLayouts pins every size and
// offset the transcription depends on.

// intelVRAMFromOS reads the memory of the device at pciAddr through its
// render node. ok is false for every reason that is not an answer.
func intelVRAMFromOS(pciAddr, driver string) (int, bool) {
	node := renderNodeFor(pciAddr)
	if node == "" {
		return 0, false
	}
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return 0, false // overwhelmingly EACCES: not in the render group
	}
	defer f.Close() //nolint:errcheck // read-only query
	var bytes uint64
	var ok bool
	switch driver {
	case "xe":
		bytes, ok = xeVRAMBytes(f.Fd())
	case "i915":
		bytes, ok = i915VRAMBytes(f.Fd())
	}
	if !ok || bytes == 0 {
		return 0, false
	}
	return int(bytes / (1 << 20)), true
}

// renderNodeFor finds /dev/dri/renderD* for the device at a PCI address.
func renderNodeFor(pciAddr string) string {
	nodes, _ := filepath.Glob("/sys/class/drm/renderD*")
	for _, n := range nodes {
		if pciAddressOf(filepath.Join(n, "device")) == pciAddr {
			return filepath.Join("/dev/dri", filepath.Base(n))
		}
	}
	return ""
}

// drmIOWR is _IOWR('d', DRM_COMMAND_BASE+cmd, size): direction read|write
// (3) in the top two bits, the argument size in the next 14, the type
// letter, then the command number.
func drmIOWR(cmd, size uintptr) uintptr {
	return (3 << 30) | (size << 16) | ('d' << 8) | (0x40 + cmd)
}

// xeDeviceQuery is struct drm_xe_device_query.
type xeDeviceQuery struct {
	extensions uint64
	query      uint32
	size       uint32
	data       uint64
	reserved   [2]uint64
}

// xeMemRegion is struct drm_xe_mem_region.
// Only its size and the offset of total_size are used (the answer is
// read out of a byte buffer), so the other fields are blank.
type xeMemRegion struct {
	_         uint16 // mem_class
	_         uint16 // instance
	_         uint32 // min_page_size
	totalSize uint64
	_         uint64    // used
	_         uint64    // cpu_visible_size
	_         uint64    // cpu_visible_used
	_         [6]uint64 // reserved
}

const (
	drmXEDeviceQuery          = 0x00 // DRM_XE_DEVICE_QUERY
	drmXEDeviceQueryMemRegion = 1    // DRM_XE_DEVICE_QUERY_MEM_REGIONS
	drmXEMemRegionClassVRAM   = 1    // DRM_XE_MEM_REGION_CLASS_VRAM
)

// xeVRAMBytes sums the VRAM regions xe reports. The query is two calls:
// the first with size 0 learns how large the answer is.
func xeVRAMBytes(fd uintptr) (uint64, bool) {
	req := xeDeviceQuery{query: drmXEDeviceQueryMemRegion}
	cmd := drmIOWR(drmXEDeviceQuery, unsafe.Sizeof(req))
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, uintptr(unsafe.Pointer(&req))); e != 0 || req.size < 8 {
		return 0, false
	}
	buf := make([]byte, req.size)
	req.data = uint64(uintptr(unsafe.Pointer(&buf[0])))
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, uintptr(unsafe.Pointer(&req))); e != 0 {
		return 0, false
	}
	return sumXEVRAM(buf), true
}

// sumXEVRAM reads struct drm_xe_query_mem_regions: a u32 count, a u32 pad,
// then that many drm_xe_mem_region.
func sumXEVRAM(buf []byte) uint64 {
	if len(buf) < 8 {
		return 0
	}
	n := int(le32(buf[0:4]))
	size := int(unsafe.Sizeof(xeMemRegion{}))
	var total uint64
	for i := 0; i < n; i++ {
		off := 8 + i*size
		if off+size > len(buf) {
			break
		}
		r := buf[off : off+size]
		if le16(r[0:2]) == drmXEMemRegionClassVRAM {
			total += le64(r[8:16])
		}
	}
	return total
}

// i915Query is struct drm_i915_query.
type i915Query struct {
	numItems uint32
	flags    uint32
	itemsPtr uint64
}

// i915QueryItem is struct drm_i915_query_item.
type i915QueryItem struct {
	queryID uint64
	length  int32
	flags   uint32
	dataPtr uint64
}

// i915MemoryRegionInfo is struct drm_i915_memory_region_info: the
// class/instance pair, a reserved u32, probed_size, unallocated_size and
// a 64-byte union.
type i915MemoryRegionInfo struct {
	_          uint16 // memory_class
	_          uint16 // memory_instance
	_          uint32 // rsvd0
	probedSize uint64
	_          uint64    // unallocated_size
	_          [8]uint64 // the union
}

const (
	drmI915Query              = 0x39 // DRM_I915_QUERY
	drmI915QueryMemoryRegions = 4    // DRM_I915_QUERY_MEMORY_REGIONS
	i915MemoryClassDevice     = 1    // I915_MEMORY_CLASS_DEVICE
)

// i915VRAMBytes sums the device-local regions i915 reports, with the
// same two-call protocol: length 0 first, then a buffer of that length.
func i915VRAMBytes(fd uintptr) (uint64, bool) {
	item := i915QueryItem{queryID: drmI915QueryMemoryRegions}
	q := i915Query{numItems: 1, itemsPtr: uint64(uintptr(unsafe.Pointer(&item)))}
	cmd := drmIOWR(drmI915Query, unsafe.Sizeof(q))
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, uintptr(unsafe.Pointer(&q))); e != 0 || item.length < 16 {
		return 0, false
	}
	buf := make([]byte, item.length)
	item.dataPtr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, cmd, uintptr(unsafe.Pointer(&q))); e != 0 || item.length < 16 {
		return 0, false
	}
	return sumI915DeviceMemory(buf), true
}

// sumI915DeviceMemory reads struct drm_i915_query_memory_regions: a u32
// count, three reserved u32, then that many region infos.
func sumI915DeviceMemory(buf []byte) uint64 {
	if len(buf) < 16 {
		return 0
	}
	n := int(le32(buf[0:4]))
	size := int(unsafe.Sizeof(i915MemoryRegionInfo{}))
	var total uint64
	for i := 0; i < n; i++ {
		off := 16 + i*size
		if off+size > len(buf) {
			break
		}
		r := buf[off : off+size]
		if le16(r[0:2]) == i915MemoryClassDevice {
			total += le64(r[8:16])
		}
	}
	return total
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func le64(b []byte) uint64 { return uint64(le32(b[0:4])) | uint64(le32(b[4:8]))<<32 }

// intelWindowsAdapters has nothing to walk on Linux; sysfs answered or
// there is no Intel GPU.
func intelWindowsAdapters(_ context.Context) []GPU { return nil }
