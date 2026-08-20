package logsink

import (
	"bytes"
	"log/slog"
	"testing"
)

// The OS secondary sink is unreachable from a test on every platform: the
// per-OS newSecondary returns nil off Windows, and on Windows it needs an
// Event Log source that only a real install registers. So the tee that
// decides what Waired writes to the Windows Event Log had no test at all,
// and the Windows body carried a `default:` arm claiming INFO went there
// (waired-agent#764). newHandler takes the secondary directly so the
// threshold — the part that is OS-independent policy — is testable here.

func recordingHandler(t *testing.T) (slog.Handler, *[]slog.Record) {
	t.Helper()
	var got []slog.Record
	primary := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := newHandler(primary, func(r slog.Record) { got = append(got, r) })
	return h, &got
}

func TestHandle_TeesWarnAndAboveOnly(t *testing.T) {
	h, got := recordingHandler(t)
	logger := slog.New(h)

	logger.Debug("quiet")
	logger.Info("routine")
	logger.Warn("careful")
	logger.Error("boom")

	if len(*got) != 2 {
		t.Fatalf("secondary saw %d records, want 2 (Warn, Error): %+v", len(*got), *got)
	}
	if lvl := (*got)[0].Level; lvl != slog.LevelWarn {
		t.Errorf("first secondary record = %v, want Warn", lvl)
	}
	if lvl := (*got)[1].Level; lvl != slog.LevelError {
		t.Errorf("second secondary record = %v, want Error", lvl)
	}
	for _, r := range *got {
		if r.Level < secondaryMinLevel {
			t.Errorf("record below secondaryMinLevel reached the secondary: %v %q", r.Level, r.Message)
		}
	}
}

// The exact boundary, as its own case: a per-OS body is allowed to assume
// it, so a change to secondaryMinLevel must fail here rather than silently
// start (or stop) writing to the Event Log.
func TestHandle_BoundaryIsWarn(t *testing.T) {
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{
		{slog.LevelWarn - 1, false},
		{slog.LevelWarn, true},
		{slog.LevelError, true},
	} {
		h, got := recordingHandler(t)
		slog.New(h).Log(t.Context(), tc.level, "probe")
		if teed := len(*got) == 1; teed != tc.want {
			t.Errorf("level %v: teed=%v, want %v", tc.level, teed, tc.want)
		}
	}
}

// WithAttrs / WithGroup return a NEW handler; dropping the secondary there
// would silently stop Event Log delivery for every logger built with
// logger.With(...), which is most of the daemon.
func TestWithAttrsAndGroupKeepTheSecondary(t *testing.T) {
	h, got := recordingHandler(t)

	slog.New(h).With("device", "dev_x").Error("boom")
	slog.New(h).WithGroup("net").Error("boom")

	if len(*got) != 2 {
		t.Fatalf("secondary saw %d records, want 2 (one per derived handler)", len(*got))
	}
}

// A platform with no secondary must hand back the primary untouched, not a
// wrapper that would call a nil func.
func TestNewHandler_NilSecondaryIsPassThrough(t *testing.T) {
	primary := slog.NewTextHandler(&bytes.Buffer{}, nil)
	if got := newHandler(primary, nil); got != primary {
		t.Errorf("newHandler(primary, nil) = %T, want the primary handler itself", got)
	}
}
