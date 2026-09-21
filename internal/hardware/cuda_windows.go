//go:build windows

package hardware

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CUDA driver API constants, from cuda.h.
const (
	cudaSuccess               = 0  // CUDA_SUCCESS
	cuDeviceAttributeIntegrat = 18 // CU_DEVICE_ATTRIBUTE_INTEGRATED
)

var (
	cudaDevicesOnce   sync.Once
	cudaDevicesCached []cudaDevice
	cudaDevicesOK     bool
)

// cudaDevicesFromOS asks nvcuda.dll for every device's integrated flag
// and total memory. ok is false when the library or any call is
// unavailable — no driver, an old driver without the _v2 entry point —
// which leaves every answer to the readings that ran before.
//
// Asked once per process: the answer is a property of the installed
// hardware, and cuInit is the one call here that is not free.
func cudaDevicesFromOS() ([]cudaDevice, bool) {
	cudaDevicesOnce.Do(func() {
		cudaDevicesCached, cudaDevicesOK = queryCUDADevices()
	})
	return cudaDevicesCached, cudaDevicesOK
}

func queryCUDADevices() ([]cudaDevice, bool) {
	dll := windows.NewLazySystemDLL("nvcuda.dll")
	if err := dll.Load(); err != nil {
		return nil, false
	}
	procs := map[string]*windows.LazyProc{}
	for _, name := range []string{"cuInit", "cuDeviceGetCount", "cuDeviceGet", "cuDeviceGetAttribute", "cuDeviceTotalMem_v2"} {
		p := dll.NewProc(name)
		if p.Find() != nil {
			return nil, false // LazyProc.Call panics on a missing export
		}
		procs[name] = p
	}
	if r, _, _ := procs["cuInit"].Call(0); r != cudaSuccess {
		return nil, false
	}
	var count int32
	if r, _, _ := procs["cuDeviceGetCount"].Call(uintptr(unsafe.Pointer(&count))); r != cudaSuccess {
		return nil, false
	}
	out := make([]cudaDevice, 0, count)
	for ordinal := int32(0); ordinal < count; ordinal++ {
		var dev int32
		if r, _, _ := procs["cuDeviceGet"].Call(uintptr(unsafe.Pointer(&dev)), uintptr(ordinal)); r != cudaSuccess {
			return nil, false
		}
		var integrated int32
		if r, _, _ := procs["cuDeviceGetAttribute"].Call(
			uintptr(unsafe.Pointer(&integrated)), cuDeviceAttributeIntegrat, uintptr(dev)); r != cudaSuccess {
			return nil, false
		}
		var total uintptr // size_t
		if r, _, _ := procs["cuDeviceTotalMem_v2"].Call(uintptr(unsafe.Pointer(&total)), uintptr(dev)); r != cudaSuccess {
			return nil, false
		}
		out = append(out, cudaDevice{integrated: integrated != 0, totalMB: int(uint64(total) / (1 << 20))})
	}
	return out, true
}
