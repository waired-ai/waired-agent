package disco

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// recordingHandler captures every slog record so a test can assert on the
// level+message of what observeOne logged.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// errSendBind is a Bind whose SendDisco returns the configured error, to
// exercise observeOne's send-failure path (waired#758). Toggling sendErr
// between direct observeOne calls is race-free — no observe loop is running.
type errSendBind struct {
	sendErr error
	inbound chan wireframe.Inbound
}

func (b *errSendBind) SendDisco(_ []byte, _ string) error               { return b.sendErr }
func (b *errSendBind) SendDiscoViaRelay(_ []byte, _, _, _ string) error { return nil }
func (b *errSendBind) DiscoInbound() <-chan wireframe.Inbound           { return b.inbound }

// TestObserveOne_SendFailureWarnsOncePerDst is the waired#758 regression: a
// udp6 host with no usable route makes every SendDisco fail, and observeOne
// used to Warn on every attempt (~2×/10s forever). It must now Warn exactly
// once per dst, demote repeats to Debug, and re-Warn after a recovery (a
// successful send clears the per-dst dedup).
func TestObserveOne_SendFailureWarnsOncePerDst(t *testing.T) {
	const msg = "disco observer: send stun_request"
	h := &recordingHandler{}
	bind := &errSendBind{
		sendErr: errors.New("network is unreachable"),
		inbound: make(chan wireframe.Inbound, 1),
	}
	priv, pub := newNodeKey(t)
	s, err := New(Config{
		SelfDeviceID:    "dev_self",
		SelfNodeKeyPriv: priv,
		SelfNodeKeyPub:  pub,
		Bind:            bind,
		Logger:          slog.New(h),
		STUNTimeout:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// Two failing sends to the SAME dst → one Warn, one suppressed Debug.
	s.observeOne(ctx, "2001:db8::1", 3478)
	s.observeOne(ctx, "2001:db8::1", 3478)
	if got := h.count(slog.LevelWarn, msg); got != 1 {
		t.Fatalf("Warn count after 2 failures to same dst = %d, want 1", got)
	}
	if got := h.count(slog.LevelDebug, msg+" (suppressed repeat)"); got != 1 {
		t.Fatalf("suppressed-Debug count = %d, want 1", got)
	}

	// A different dst (second DiscoUDPPort) gets its own single Warn.
	s.observeOne(ctx, "2001:db8::1", 3479)
	if got := h.count(slog.LevelWarn, msg); got != 2 {
		t.Fatalf("Warn count after a new dst = %d, want 2", got)
	}

	// Recovery: a successful send to the first dst clears its dedup, so a
	// later failure to it re-Warns once.
	bind.sendErr = nil
	s.observeOne(ctx, "2001:db8::1", 3478) // succeeds → resets dedup (then times out)
	bind.sendErr = errors.New("network is unreachable")
	s.observeOne(ctx, "2001:db8::1", 3478) // fails again → re-Warn
	if got := h.count(slog.LevelWarn, msg); got != 3 {
		t.Fatalf("Warn count after recovery+refailure = %d, want 3", got)
	}
}
