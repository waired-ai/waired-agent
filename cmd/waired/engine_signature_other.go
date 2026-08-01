//go:build !darwin

package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/setup"
)

// engineBundleSignatureProblem is macOS-only.
//
// Windows and Linux install the engine as a plain directory of files: there is
// no signed application bundle, so there is no resource seal to invalidate and
// nothing for a signature check to catch. Windows has its own "installed but
// unusable" predicate — engineIncomplete, keyed on the completion marker beside
// ollama.exe — and it stays the authority there. Always nil. (#330)
func engineBundleSignatureProblem(context.Context, setup.OllamaDetection) error { return nil }
