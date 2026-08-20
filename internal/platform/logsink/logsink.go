// Package logsink wraps a primary slog.Handler with an OS-native
// secondary sink for high-severity records.
//
// Why this exists: on Windows when the agent runs under the SCM,
// stderr is closed. slog records written to a NewJSONHandler(os.Stderr,
// ...) are dropped silently. We mirror Warn / Error / higher levels
// to the Application Event Log so an operator can `Get-WinEvent
// -ProviderName waired-agent` and see what went wrong.
//
// On Linux + macOS the primary stderr handler is sufficient — systemd
// captures unit stderr to journald, launchd captures to a plist-named
// file or unified logging — so the per-OS body returns a nil secondary
// and this wrapper degrades to a transparent pass-through.
package logsink

import (
	"context"
	"log/slog"
)

// New wraps primary with an OS-native secondary sink. serviceName is
// the Event Log source / launchd label; on platforms without a
// secondary sink it is ignored.
func New(primary slog.Handler, serviceName string) slog.Handler {
	return newHandler(primary, newSecondary(serviceName))
}

// newHandler is New with the OS secondary supplied directly. Split out so
// the threshold below can be exercised on any OS: newSecondary returns nil
// everywhere but Windows, and even there it needs an Event Log source that
// only a real install registers, so the tee is otherwise untestable
// (waired-agent#764).
func newHandler(primary slog.Handler, sec func(slog.Record)) slog.Handler {
	if sec == nil {
		return primary
	}
	return &handler{primary: primary, secondary: sec}
}

// handler tees Warn+ records to the OS secondary while delegating
// everything else (and the WithAttrs / WithGroup wrappers) to the
// primary.
type handler struct {
	primary   slog.Handler
	secondary func(slog.Record)
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level)
}

// secondaryMinLevel is the one place the OS-secondary threshold lives.
// Handle is the only caller of handler.secondary, so a per-OS body may
// assume every record it is handed is at or above this level: it maps the
// level onto an OS severity and does not re-filter — and must not describe
// a level it can never be given (waired-agent#764).
const secondaryMinLevel = slog.LevelWarn

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= secondaryMinLevel {
		h.secondary(r)
	}
	return h.primary.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{primary: h.primary.WithAttrs(attrs), secondary: h.secondary}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{primary: h.primary.WithGroup(name), secondary: h.secondary}
}
