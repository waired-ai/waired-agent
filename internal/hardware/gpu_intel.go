package hardware

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// detectIntel finds Intel GPUs (waired-agent#1483). Until this existed
// there was no Intel detector at all, so every Intel GPU — a laptop's Arc
// iGPU, a desktop's Arc B580 — was invisible and its host keyed as
// cpu-none.
//
// Detection alone would have made that worse, not better: a detected iGPU
// used to make the host ClassDiscrete with a small carve-out as its VRAM
// (docs/knowledges/20260805/1610 §4). It is safe now because the engine
// leaves every Intel iGPU off by default and the profiler sets it aside
// (engine_gpus.go), so an integrated one changes nothing the host is
// described by, and only a discrete card whose memory size is known
// joins the GPUs the host is described by.
//
// Linux reads sysfs (and, where the render node opens, the driver's own
// memory query); Windows walks the display-adapter registry as it does
// for AMD. Integrated or discrete comes from the kernel's device-ID table
// on both, so the same card is classified the same way on either OS.
func detectIntel(ctx context.Context) ([]GPU, Accelerators, error) {
	gpus := readIntelSysfs(sysfsRoot, intelVRAMFromOS)
	if len(gpus) == 0 {
		gpus = intelWindowsAdapters(ctx)
	}
	for i := range gpus {
		applyIntelPart(&gpus[i])
	}
	return gpus, Accelerators{}, nil
}

// applyIntelPart sets what the kernel's table knows about a device:
// integrated or discrete. An ID the table does not carry is left as the
// detector found it — unknown — for the per-OS reading to settle.
func applyIntelPart(g *GPU) {
	if p, ok := intelPartFor(g.PCIID); ok {
		g.Integrated, g.IntegratedKnown = !p.discrete, true
	}
}

// readIntelSysfs lists every GPU bound to the i915 or xe driver.
//
// vram reads a discrete card's memory size through the driver, which
// needs the render node; it is handed in so a test can supply a reading
// and so that platforms without one pass a stub.
func readIntelSysfs(root string, vram func(pciAddr, driver string) (int, bool)) []GPU {
	cards, _ := filepath.Glob(filepath.Join(root, "sys", "class", "drm", "card[0-9]*"))
	var out []GPU
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue
		}
		dev := filepath.Join(card, "device")
		if readTrim(filepath.Join(dev, "vendor")) != "0x8086" {
			continue
		}
		if !strings.HasPrefix(readTrim(filepath.Join(dev, "class")), "0x03") {
			continue
		}
		drv, err := os.Readlink(filepath.Join(dev, "driver"))
		if err != nil {
			continue
		}
		driver := filepath.Base(drv)
		if driver != "i915" && driver != "xe" {
			continue
		}
		deviceRaw := readTrim(filepath.Join(dev, "device"))
		g := GPU{
			Vendor: "intel",
			PCIID:  PCIIDFromSysfs("0x8086", deviceRaw),
		}
		g.Model = "Intel GPU " + g.PCIID
		// Only a discrete card has memory of its own to read; asking an
		// integrated one would report a slice of system RAM.
		if p, ok := intelPartFor(g.PCIID); !ok || p.discrete {
			if vram != nil {
				if mb, ok := vram(pciAddressOf(dev), driver); ok {
					g.VRAMTotalMB = mb
				}
			}
		}
		out = append(out, g)
	}
	return out
}
