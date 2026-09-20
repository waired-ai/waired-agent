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
	modPdh                   = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery         = modPdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter = modPdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData  = modPdh.NewProc("PdhCollectQueryData")
	procPdhGetFormatted      = modPdh.NewProc("PdhGetFormattedCounterValue")

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

// pagesOutCounter is the pagefile write rate, in pages per second.
//
// Windows publishes no cumulative count of bytes written to the pagefile
// that a caller can difference, so this one platform reads a rate the OS has
// already computed. PdhAddEnglishCounter rather than PdhAddCounter: counter
// paths are LOCALISED, and the hosts this product runs on include Japanese
// Windows, where the localised path would not resolve.
const pagesOutCounter = `\Memory\Pages Output/sec`

const pdhFmtDouble = 0x00000200

// pdhFmtCounterValue is PDH_FMT_COUNTERVALUE. The union after CStatus is
// 8-byte aligned, hence the explicit padding.
type pdhFmtCounterValue struct {
	CStatus uint32
	_       uint32
	Double  float64
}

type windowsSampler struct {
	mu     sync.Mutex
	handle windows.Handle
	closed bool

	query    uintptr
	pagesOut uintptr
	pdhOK    bool
}

func newPlatformSampler() platformSampler {
	s := &windowsSampler{}
	// One handle for the life of the sampler. The object is system-wide and
	// the handle is only a way to ask about it, so creating one per sample
	// would be pure cost.
	r, _, _ := procCreateMemoryResourceNotifica.Call(uintptr(lowMemoryResourceNotification))
	s.handle = windows.Handle(r) // 0 on failure, which facts() reports as "could not query"
	s.openPdh()
	return s
}

// openPdh prepares the pagefile-rate counter. Failure is not fatal: without
// it the surge half of the rule is simply unavailable on this host, and the
// low-memory notification still carries the rest.
func (s *windowsSampler) openPdh() {
	var q uintptr
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return
	}
	path, err := windows.UTF16PtrFromString(pagesOutCounter)
	if err != nil {
		return
	}
	var h uintptr
	if r, _, _ := procPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0,
		uintptr(unsafe.Pointer(&h))); r != 0 {
		return
	}
	// A rate counter needs a first collection to difference the next against.
	procPdhCollectQueryData.Call(q)
	s.query, s.pagesOut, s.pdhOK = q, h, true
}

// swapOutMBPerSec reads the pagefile write rate. -1 means it could not be
// read, which is a different fact from a rate of zero.
func (s *windowsSampler) swapOutMBPerSec() float64 {
	if !s.pdhOK {
		return -1
	}
	if r, _, _ := procPdhCollectQueryData.Call(s.query); r != 0 {
		return -1
	}
	var v pdhFmtCounterValue
	if r, _, _ := procPdhGetFormatted.Call(s.pagesOut, pdhFmtDouble, 0,
		uintptr(unsafe.Pointer(&v))); r != 0 {
		return -1
	}
	return v.Double * 4096 / (1 << 20)
}

func (s *windowsSampler) facts() Facts {
	f := Facts{
		WindowsLowSignaled: s.querySignaled(),
		// PDH gives a rate directly, so there is nothing for the Sampler to
		// difference; -1 keeps it from trying.
		SwapOutTotalMB:  -1,
		SwapOutMBPerSec: s.swapOutMBPerSec(),
	}

	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	if r, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		f.WindowsErr = fmt.Errorf("mempressure: GlobalMemoryStatusEx: %w", callErr)
		return f
	}
	const mb = 1 << 20
	f.TotalMB = st.TotalPhys / mb
	f.AvailMB = st.AvailPhys / mb
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
