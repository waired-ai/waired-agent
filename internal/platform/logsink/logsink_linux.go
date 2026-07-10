//go:build linux

package logsink

import "log/slog"

// newSecondary returns nil on Linux because systemd already captures
// the unit's stdout/stderr into journald. Adding a second sink would
// duplicate every Warn/Error record.
func newSecondary(_ string) func(slog.Record) { return nil }
