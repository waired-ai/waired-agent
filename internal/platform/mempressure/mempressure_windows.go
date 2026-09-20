package mempressure

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The two calls this package needs. x/sys/windows wraps
// GlobalMemoryStatusEx, but not the memory resource notification pair, so
// those are resolved the way the rest of this repo resolves kernel32 entry
// points (internal/hardware/profiler_windows.go).
var (
	modKernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx         = modKernel32.NewProc("GlobalMemoryStatusEx")
	procCreateMemoryResourceNotifica = modKernel32.NewProc("CreateMemoryResourceNotification")
	procQueryMemoryResourceNotificat = modKernel32.NewProc("QueryMemoryResourceNotification")
)

// memoryStatusEx is MEMORYSTATUSEX. x/sys/windows does not export it, and
// internal/hardware's copy is unexported in its own package, so this is the
// second declaration of the same Win32 struct in this repo — the fields are
// fixed by the API and neither copy may reorder them.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// lowMemoryResourceNotification is MEMORY_RESOURCE_NOTIFICATION_TYPE's
// LowMemoryResourceNotification: "available physical memory is running low".
const lowMemoryResourceNotification = 0

type windowsSampler struct {
	mu     sync.Mutex
	handle windows.Handle
	closed bool
}

func newPlatformSampler() platformSampler {
	s := &windowsSampler{}
	// One handle for the life of the sampler. The object is system-wide and
	// the handle is only a way to ask about it, so creating one per sample
	// would be pure cost.
	r, _, _ := procCreateMemoryResourceNotifica.Call(uintptr(lowMemoryResourceNotification))
	s.handle = windows.Handle(r) // 0 on failure, which facts() reports as "could not query"
	return s
}

func (s *windowsSampler) facts() Facts {
	f := Facts{WindowsLowSignaled: s.querySignaled()}

	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	if r, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		f.WindowsErr = fmt.Errorf("mempressure: GlobalMemoryStatusEx: %w", callErr)
		return f
	}
	const mb = 1 << 20
	f.WindowsTotalPhysMB = st.TotalPhys / mb
	f.WindowsAvailPhysMB = st.AvailPhys / mb
	return f
}

// querySignaled returns 1 signaled, 0 not signaled, -1 the query failed.
// The three are distinct on purpose: an unreadable notification must not
// read as "memory is fine".
func (s *windowsSampler) querySignaled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == 0 || s.closed {
		return -1
	}
	var state int32
	r, _, _ := procQueryMemoryResourceNotificat.Call(
		uintptr(s.handle), uintptr(unsafe.Pointer(&state)))
	if r == 0 {
		return -1
	}
	if state != 0 {
		return 1
	}
	return 0
}

func (s *windowsSampler) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.handle == 0 {
		s.closed = true
		return nil
	}
	s.closed = true
	h := s.handle
	s.handle = 0
	return windows.CloseHandle(h)
}
