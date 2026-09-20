//go:build !linux

package hardware

// pciIDFromOS has no post-pass to do off Linux.
//
// Windows sets PCIID where it already has the string — readDisplayAdapter
// reads MatchingDeviceId for the vendor filter anyway, so parsing the
// pair out of it there costs nothing and avoids a second registry walk.
// macOS has no PCI bus on the parts this repo runs on; an Apple Silicon
// GPU is named by its chip.
func pciIDFromOS(_ *Profile, _ int) string { return "" }
