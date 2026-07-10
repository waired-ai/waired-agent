package hardware

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestComposeDetectors_MergesAccelerators verifies the composite
// OR-merges Accelerators flags across vendors so a host with both an
// Nvidia and an AMD card lights up both CUDA and ROCm.
func TestComposeDetectors_MergesAccelerators(t *testing.T) {
	detectors := []VendorDetector{
		func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "nvidia", Model: "RTX 4090"}}, Accelerators{CUDA: true}, nil
		},
		func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "Radeon Pro W7900"}}, Accelerators{ROCm: true}, nil
		},
	}
	gpus, accel, err := composeDetectors(context.Background(), detectors)
	if err != nil {
		t.Fatalf("composeDetectors err = %v, want nil", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("len(gpus) = %d, want 2 (%+v)", len(gpus), gpus)
	}
	if !accel.CUDA || !accel.ROCm {
		t.Errorf("Accelerators = %+v, want CUDA && ROCm both true", accel)
	}
	if accel.Metal {
		t.Errorf("Metal should remain false when no Metal detector ran, got %+v", accel)
	}
}

// TestComposeDetectors_MergesErrors verifies both detector errors
// reach the caller via errors.Join so profiler.Profile surfaces them
// in Profile.Errors.
func TestComposeDetectors_MergesErrors(t *testing.T) {
	detectors := []VendorDetector{
		func(context.Context) ([]GPU, Accelerators, error) {
			return nil, Accelerators{}, errors.New("first vendor failed")
		},
		func(context.Context) ([]GPU, Accelerators, error) {
			return nil, Accelerators{}, errors.New("second vendor failed")
		},
	}
	_, _, err := composeDetectors(context.Background(), detectors)
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first vendor failed") || !strings.Contains(msg, "second vendor failed") {
		t.Errorf("joined error should contain both vendor messages, got %q", msg)
	}
}

// TestComposeDetectors_PartialFailure verifies that a failing
// detector does NOT suppress a succeeding detector's results — the
// GPU list still contains the AMD card and the error surfaces
// separately.
func TestComposeDetectors_PartialFailure(t *testing.T) {
	detectors := []VendorDetector{
		func(context.Context) ([]GPU, Accelerators, error) {
			return nil, Accelerators{}, errors.New("nvidia detection failed")
		},
		func(context.Context) ([]GPU, Accelerators, error) {
			return []GPU{{Vendor: "amd", Model: "Radeon Pro W7900"}}, Accelerators{ROCm: true}, nil
		},
	}
	gpus, accel, err := composeDetectors(context.Background(), detectors)
	if err == nil {
		t.Fatal("expected error from first detector to surface")
	}
	if !strings.Contains(err.Error(), "nvidia detection failed") {
		t.Errorf("error should contain nvidia message, got %v", err)
	}
	if len(gpus) != 1 || gpus[0].Vendor != "amd" {
		t.Errorf("AMD GPU should be preserved despite Nvidia failure, got %+v", gpus)
	}
	if !accel.ROCm || accel.CUDA {
		t.Errorf("only ROCm should be true (AMD only), got %+v", accel)
	}
}

// TestComposeDetectors_EmptyList covers the no-vendor case (e.g.
// future build that excludes all detectors) — should return empty
// results without panicking.
func TestComposeDetectors_EmptyList(t *testing.T) {
	gpus, accel, err := composeDetectors(context.Background(), nil)
	if err != nil {
		t.Errorf("empty detectors err = %v, want nil", err)
	}
	if len(gpus) != 0 {
		t.Errorf("empty detectors gpus = %+v, want empty", gpus)
	}
	if accel.CUDA || accel.ROCm || accel.Metal {
		t.Errorf("empty detectors accel = %+v, want zero", accel)
	}
}
