//go:build darwin

package hardware

import "runtime"

// macOS's answer to the question in integrated.go.
//
// THE FACT: the architecture. Every Apple Silicon part ships one pool;
// Apple has never shipped an arm64 Mac with a discrete GPU, and the
// Metal API's own predicate for this (MTLDevice.hasUnifiedMemory) is
// true on all of them.
//
// WHY NOT ASK METAL. Reading hasUnifiedMemory needs Objective-C, and
// every agent binary is built CGO_ENABLED=0 (Makefile). Nothing in
// system_profiler exposes it either: SPDisplaysDataType simply omits the
// VRAM keys on Apple Silicon, and absence of a key is not a contract.
//
// WHY THE TEST IS THE ARCHITECTURE AND NOT THE OPERATING SYSTEM. It
// would be wrong today to read "macOS" as "unified". macOS 26 still
// supports the Mac Pro (2019), MacBook Pro 16-inch (2019) and iMac
// (2020), all of which carry discrete AMD GPUs with real dedicated VRAM
// — a Mac Pro can hold 64 GB of HBM2 that is emphatically not system
// memory. Those are amd64, so the architecture test excludes them and
// the OS test would not. This file says so explicitly because the
// shorter claim is the one a future reader will be tempted to simplify
// to. ollama draws the line in the same place, and says why:
//
//	"we only infer 'integrated' here for cases where the contract is
//	 strong: explicit Vulkan UMA metadata, or the single Apple Silicon
//	 Metal device. Other backends stay unclassified unless discovery
//	 provides a stronger signal. That keeps scheduling conservative
//	 instead of guessing from device names."
//	                              -- ollama discover/llama_server.go
//
// An Intel Mac is therefore UNKNOWN rather than "discrete": its GPU may
// be an Intel integrated part or an AMD discrete one, and nothing
// reachable from here tells them apart. Unknown leaves the behaviour
// this platform has today.
func integratedFromOS(_ *Profile, _ int) integration {
	if runtime.GOARCH != "arm64" {
		return integrationUnknown()
	}
	return integratedKnown(true)
}
