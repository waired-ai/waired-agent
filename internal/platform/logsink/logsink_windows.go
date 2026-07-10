//go:build windows

package logsink

import (
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/sys/windows/svc/eventlog"
)

// newSecondary opens the Application Event Log source and returns a
// closure that writes each Warn/Error/higher slog.Record there. If
// eventlog.Open fails (source not registered yet, e.g. interactive
// debug run) returns nil so the wrapper degrades to pass-through.
func newSecondary(source string) func(slog.Record) {
	if source == "" {
		return nil
	}
	elog, err := eventlog.Open(source)
	if err != nil {
		// The source is normally registered at install time
		// (eventlog.InstallAsEventCreate). Interactive runs lack the
		// source, but stderr is open in that case so the primary sink
		// is sufficient.
		return nil
	}

	var mu sync.Mutex
	return func(r slog.Record) {
		// Build a single-line message with the level, the message,
		// and any record attributes (key=value). Event Log entries
		// are flat strings, not structured.
		var b strings.Builder
		b.WriteString(r.Level.String())
		b.WriteByte(' ')
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteByte(' ')
			b.WriteString(a.Key)
			b.WriteByte('=')
			b.WriteString(a.Value.String())
			return true
		})
		msg := b.String()

		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Level >= slog.LevelError:
			_ = elog.Error(1, msg)
		case r.Level >= slog.LevelWarn:
			_ = elog.Warning(1, msg)
		default:
			_ = elog.Info(1, msg)
		}
	}
}
