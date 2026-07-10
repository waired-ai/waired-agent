//go:build darwin

package logsink

import "log/slog"

// newSecondary returns nil on darwin. A future implementation could
// emit os_log entries via the unified logging system, but for now we
// rely on launchd's StandardErrorPath capture.
func newSecondary(_ string) func(slog.Record) { return nil }
