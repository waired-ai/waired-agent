//go:build !darwin

package hardware

import "context"

// detectApple is a no-op on non-Darwin platforms. The Apple Silicon
// GPU only exists on macOS, so there is nothing to probe on Linux /
// Windows / other UNIX hosts.
func detectApple(_ context.Context) ([]GPU, Accelerators, error) {
	return nil, Accelerators{}, nil
}
