//go:build linux

package hardware

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Kernel-exported inventories, both readable by an unprivileged service
// user (the systemd unit runs as `waired` with NoNewPrivileges).
const (
	// procNvidiaGPUsDir has one directory per GPU the NVIDIA kernel
	// module bound, each with an `information` file. Its existence means
	// the driver is loaded, which is the fact the fallback needs.
	procNvidiaGPUsDir = "/proc/driver/nvidia/gpus"
	// sysfsPCIDevicesDir is the PCI inventory, present with or without a
	// vendor driver. It can only say "an NVIDIA display adapter exists".
	sysfsPCIDevicesDir = "/sys/bus/pci/devices"
)

// nvidiaPCIVendorHex is NVIDIA's PCI vendor ID as sysfs prints it.
const nvidiaPCIVendorHex = "0x10de"

// nvidiaFallback answers "is an NVIDIA driver alive here" when
// nvidia-smi could not.
//
// Linux ships nvidia-smi in /usr/bin, so this path is rarer than the
// Windows one — but the service's PATH is not the user's here either,
// and the repo's parity rule is that a fix is not done until the other
// OSes are covered. There is no libnvidia-ml equivalent of the Windows
// NVML layer: loading a shared library needs cgo, and every agent binary
// is built CGO_ENABLED=0. The kernel's own inventories answer the
// presence question without it.
//
// VRAM is not readable from either source, so devices come back with
// VRAMTotalMB == 0 and the caller warns; downstream that degrades to
// "budget unknown" (hostfit.OllamaResident / EstimateOllamaDecode both
// decline to judge), never to "no GPU".
func nvidiaFallback(_ context.Context) nvidiaFallbackResult {
	if gpus := nvidiaProcDevices(procNvidiaGPUsDir); len(gpus) > 0 {
		return nvidiaFallbackResult{GPUs: gpus, AdapterSeen: true, Source: "procfs"}
	}
	if nvidiaPCIAdapterPresent(sysfsPCIDevicesDir) {
		return nvidiaFallbackResult{
			AdapterSeen: true,
			Source:      "sysfs PCI; the NVIDIA kernel module does not appear to be loaded",
		}
	}
	return nvidiaFallbackResult{}
}

// nvidiaProcDevices reads one GPU per directory under root (the shape of
// /proc/driver/nvidia/gpus/<bus-id>/information).
func nvidiaProcDevices(root string) []GPU {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []GPU
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "information"))
		if err != nil {
			continue
		}
		gpu := parseNvidiaProcInformation(string(b))
		if gpu.Model == "" {
			continue
		}
		out = append(out, gpu)
	}
	return out
}

// parseNvidiaProcInformation extracts what the kernel's `information`
// file carries: the marketing model name and the GPU UUID. There is no
// VRAM figure in it, by design of the file — VRAMTotalMB stays 0.
func parseNvidiaProcInformation(s string) GPU {
	gpu := GPU{Vendor: "nvidia"}
	for _, line := range strings.Split(s, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Model":
			gpu.Model = value
		case "GPU UUID":
			gpu.UUID = value
		}
	}
	return gpu
}

// nvidiaPCIAdapterPresent reports whether the PCI inventory under root
// lists an NVIDIA display controller (class 0x03xxxx). This is the
// "adapter present, driver missing" signal — enough to make the miss
// loud, never enough to claim a usable GPU.
func nvidiaPCIAdapterPresent(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		vendor, err := os.ReadFile(filepath.Join(root, e.Name(), "vendor"))
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(vendor)), nvidiaPCIVendorHex) {
			continue
		}
		class, err := os.ReadFile(filepath.Join(root, e.Name(), "class"))
		if err != nil {
			continue
		}
		// 0x030000 VGA, 0x030200 3D controller — the headless compute
		// cards report the latter.
		if strings.HasPrefix(strings.TrimSpace(string(class)), "0x03") {
			return true
		}
	}
	return false
}
