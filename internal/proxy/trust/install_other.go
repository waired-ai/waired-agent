//go:build !linux && !windows && !darwin

package trust

import "errors"

// errUnsupported is returned by every trust operation on a platform without a
// trust-store integration. Linux, Windows, and macOS have native
// implementations; cmd/waired surfaces this as a clear "not yet supported on
// this OS".
var errUnsupported = errors.New("trust: OS trust-store integration is only implemented on Linux, Windows, and macOS")

func InstallCA([]byte) error          { return errUnsupported }
func UninstallCA() error              { return errUnsupported }
func InstallNodeExtraCA(string) error { return errUnsupported }
func UninstallNodeExtraCA() error     { return errUnsupported }
