//go:build !windows

package hardware

// cudaDevicesFromOS has no answer off Windows: loading libcuda needs cgo,
// which the agent builds without (cuda_facts.go).
func cudaDevicesFromOS() ([]cudaDevice, bool) { return nil, false }
