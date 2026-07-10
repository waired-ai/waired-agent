//go:build !darwin || !cgo

package tray

// setActivationPolicyAccessory is a no-op everywhere except darwin+cgo.
// On Linux the tray has no Dock-equivalent, and on Windows the clean
// tray-only UX is achieved at link time via `-H windowsgui` (see
// build-tray-windows in the Makefile). The `!cgo` arm keeps the package
// compilable under `GOOS=darwin CGO_ENABLED=0` (e.g. cross-vet), where
// the cgo implementation in activation_darwin.go is excluded.
func setActivationPolicyAccessory() {}
