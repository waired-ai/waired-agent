//go:build !windows && !linux && !darwin

package elevation

// isElevated is conservatively false on any OS without a known elevation
// model, so a machine-wide action gated on IsElevated never runs on an
// unverified privilege assumption.
func isElevated() bool { return false }
