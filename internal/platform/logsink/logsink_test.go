package logsink_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/logsink"
)

// TestNew_PassThroughOnUnix verifies that on platforms without a
// secondary sink (Linux/Darwin) New returns the primary handler
// unchanged. On Windows the wrapper is always installed (even if
// eventlog.Open fails, secondary is nil and we still return primary).
func TestNew_DelegatesEverythingToPrimary(t *testing.T) {
	var buf bytes.Buffer
	primary := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	wrapped := logsink.New(primary, "waired-agent-test")

	logger := slog.New(wrapped)
	logger.Info("hello", "k", "v")
	logger.Warn("careful", "code", 42)
	logger.Error("boom", "err", "broken")

	out := buf.String()
	for _, want := range []string{"hello", "k=v", "careful", "code=42", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("primary output missing %q:\n%s", want, out)
		}
	}
}

// TestNew_EnabledRespectsPrimary checks that level filtering on the
// primary handler is preserved through the wrapper — useful for the
// LevelInfo/Debug filter the agent applies at startup.
func TestNew_EnabledRespectsPrimary(t *testing.T) {
	primary := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})
	wrapped := logsink.New(primary, "waired-agent-test")

	if wrapped.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("wrapper should report Info as disabled when primary's threshold is Error")
	}
	if !wrapped.Enabled(context.Background(), slog.LevelError) {
		t.Error("wrapper should report Error as enabled when primary's threshold is Error")
	}
}
