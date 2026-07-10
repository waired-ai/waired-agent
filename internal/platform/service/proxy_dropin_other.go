//go:build !linux && !darwin

package service

import "errors"

// errProxyDropInUnsupported is returned on Windows, which had no systemd /
// launchd proxy units to remove (the retired MITM proxy converged OS state in
// the LocalSystem agent itself). The legacy-cleanup path ignores this error.
var errProxyDropInUnsupported = errors.New("service: proxy drop-in lifecycle is only implemented on Linux and macOS")

// RemoveProxyDropIn is a no-op stub on Windows (no service units to remove).
func RemoveProxyDropIn() error { return errProxyDropInUnsupported }
