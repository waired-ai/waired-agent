package download

import "fmt"

// CheckDiskSpace fails when freeBytes < requiredBytes. Bootstrap / CLI /
// the install-time bundled-model pre-flight (#517) call this before
// kicking off a Pull so a multi-GB download doesn't ENOSPC halfway
// through. A non-positive requiredBytes is treated as "unknown size" and
// passes (we never reject on an unknown requirement).
//
// Lives in its own platform-agnostic file (hf.go is //go:build !windows)
// so callers on every OS — e.g. setup.Deploy's pre-pull guard — can use
// it.
func CheckDiskSpace(freeBytes, requiredBytes int64) error {
	if requiredBytes <= 0 {
		return nil
	}
	if freeBytes < requiredBytes {
		return fmt.Errorf("download: insufficient disk space: need %.1f GB, have %.1f GB",
			float64(requiredBytes)/(1024*1024*1024),
			float64(freeBytes)/(1024*1024*1024))
	}
	return nil
}
