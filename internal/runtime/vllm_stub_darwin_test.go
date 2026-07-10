//go:build darwin

package runtime

import (
	"context"
	"errors"
	"testing"
)

// TestVLLMInstallerStub_DarwinRefuses confirms the macOS stub returns
// ErrVLLMUnsupportedOnDarwin for Install / Uninstall, and reports
// "no active install" so the engine picker silently falls through to
// Ollama (Metal) on Apple Silicon hosts.
func TestVLLMInstallerStub_DarwinRefuses(t *testing.T) {
	inst := NewVLLMInstaller()

	if _, ok := inst.Active(); ok {
		t.Fatalf("Active() = (_, true); want (_, false) on darwin stub")
	}

	_, err := inst.Install(context.Background(), InstallOpts{}, func(InstallProgress) {})
	if !errors.Is(err, ErrVLLMUnsupportedOnDarwin) {
		t.Errorf("Install err = %v; want ErrVLLMUnsupportedOnDarwin", err)
	}

	if err := inst.Uninstall(context.Background(), ""); !errors.Is(err, ErrVLLMUnsupportedOnDarwin) {
		t.Errorf("Uninstall err = %v; want ErrVLLMUnsupportedOnDarwin", err)
	}
}

// TestVLLMPinnedVersion_DarwinExposed confirms the constant is visible
// on darwin so cmd/waired CLI help text renders the version even on a
// host where Install will refuse.
func TestVLLMPinnedVersion_DarwinExposed(t *testing.T) {
	if VLLMPinnedVersion == "" {
		t.Fatalf("VLLMPinnedVersion must be non-empty on darwin stub")
	}
}
