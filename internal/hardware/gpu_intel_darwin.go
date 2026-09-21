//go:build darwin

package hardware

import "context"

// Intel Macs' GPUs are not engine GPUs: ollama's macOS build has only
// Metal on Apple Silicon or the CPU (internal/runtime/ollama_backend.go),
// so there is nothing for an Intel detector to report here.
func intelWindowsAdapters(_ context.Context) []GPU { return nil }

func intelVRAMFromOS(string, string) (int, bool) { return 0, false }
