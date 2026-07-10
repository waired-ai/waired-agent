//go:build !linux

package main

import "io"

// ensureTrayHostExtension is a Linux-only concern (the GNOME AppIndicator SNI
// host extension). macOS and Windows have native tray hosts, so this is a no-op.
func ensureTrayHostExtension(io.Writer) {}
