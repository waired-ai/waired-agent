//go:build !linux && !darwin && !windows

package service

import "errors"

// Stub implementation for any GOOS that isn't explicitly supported
// above. Keeps `go build ./...` working on exotic targets while
// telling operators that their platform isn't wired up.

var errUnsupportedOS = errors.New("service: not supported on this OS")

func newManager() Manager { return stubManager{} }

func osDispatchInteractive(_ []string, _ RunHook) (bool, int) {
	return false, 0
}

type stubManager struct{}

func (stubManager) Install(Config) error { return errUnsupportedOS }
func (stubManager) Uninstall() error     { return errUnsupportedOS }
func (stubManager) Start([]string) error { return errUnsupportedOS }
func (stubManager) Stop() error          { return errUnsupportedOS }

// Installed always reports false on unsupported platforms.
func Installed() bool { return false }

// StartHint has no platform-native start command on unsupported OSes.
func StartHint() string { return "" }

// FixStateOwnership is a no-op on unsupported platforms.
func FixStateOwnership(string) error { return nil }
