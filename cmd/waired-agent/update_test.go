package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/update"
)

func newTestController(check func(ctx context.Context, current string) (update.Result, error)) (*updateController, *time.Time) {
	clock := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	c := &updateController{
		current:       "1.2.3",
		check:         check,
		now:           func() time.Time { return clock },
		notifyEnabled: true, // default ON, mirrors newUpdateController
	}
	return c, &clock
}

func TestUpdateController_CheckAvailable(t *testing.T) {
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		return update.Result{Available: true, Current: current, Latest: "1.4.0"}, nil
	})
	st, err := c.Check(context.Background(), management.UpdateCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != management.UpdatePhaseAvailable || !st.Available || st.LatestVersion != "1.4.0" {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st.CurrentVersion != "1.2.3" || st.CheckedAt == "" {
		t.Fatalf("missing current/checkedAt: %+v", st)
	}
}

func TestUpdateController_CacheAndForce(t *testing.T) {
	var calls int
	var mu sync.Mutex
	c, clock := newTestController(func(_ context.Context, current string) (update.Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return update.Result{Available: false, Current: current, Latest: "1.2.3"}, nil
	})

	if _, err := c.Check(context.Background(), management.UpdateCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	// Second call within TTL must hit the cache (no extra resolver call).
	if _, err := c.Check(context.Background(), management.UpdateCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 resolver call within TTL, got %d", calls)
	}
	// Force bypasses the cache.
	if _, err := c.Check(context.Background(), management.UpdateCheckRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 resolver calls after Force, got %d", calls)
	}
	// Past the TTL, a non-forced call re-checks.
	*clock = clock.Add(updateCacheTTL + time.Minute)
	if _, err := c.Check(context.Background(), management.UpdateCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 resolver calls after TTL expiry, got %d", calls)
	}
}

func TestUpdateController_ErrorKeepsCache(t *testing.T) {
	var fail bool
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		if fail {
			return update.Result{}, errors.New("github unreachable")
		}
		return update.Result{Available: true, Current: current, Latest: "1.4.0"}, nil
	})
	// Seed a good cache.
	if _, err := c.Check(context.Background(), management.UpdateCheckRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	// A forced check that fails reports the error but must not clobber cache.
	fail = true
	st, err := c.Check(context.Background(), management.UpdateCheckRequest{Force: true})
	if err != nil {
		t.Fatalf("Check should fold feed errors into status, got Go err: %v", err)
	}
	if st.Phase != management.UpdatePhaseError || st.Error == "" {
		t.Fatalf("expected error phase, got %+v", st)
	}
	// Status() still returns the last good result.
	got, _ := c.Status(context.Background())
	if got.Phase != management.UpdatePhaseAvailable || !got.Available {
		t.Fatalf("error clobbered cached status: %+v", got)
	}
}

func TestUpdateController_StatusBeforeCheck(t *testing.T) {
	c := newUpdateController(t.TempDir())
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != management.UpdatePhaseIdle || st.Available {
		t.Fatalf("expected idle status before any check, got %+v", st)
	}
	if st.ApplyMethod == "" {
		t.Fatalf("ApplyMethod should be populated, got %+v", st)
	}
	// Default ON: a host that has never touched the toggle is prompted.
	if !st.NotifyEnabled {
		t.Fatalf("NotifyEnabled should default to true, got %+v", st)
	}
}

// SetNotify persists to <state-dir>/runtime/desired-update-notify and the
// preference is reflected on every status the controller returns, regardless
// of when the cached check ran.
func TestUpdateController_SetNotifyPersists(t *testing.T) {
	dir := t.TempDir()
	c := newUpdateController(dir)

	st, err := c.SetNotify(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if st.NotifyEnabled {
		t.Fatalf("SetNotify(false) should report NotifyEnabled=false, got %+v", st)
	}
	// Persisted on disk and survives a fresh controller (daemon restart).
	got, err := state.ReadDesiredUpdateNotify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.UpdateNotifyOff {
		t.Fatalf("desired-update-notify not persisted: got %q", got)
	}
	if c2 := newUpdateController(dir); c2.notifyEnabled {
		t.Fatalf("reloaded controller should be notify-off")
	}

	// Status overlays the live preference.
	status, _ := c.Status(context.Background())
	if status.NotifyEnabled {
		t.Fatalf("Status should reflect notify-off, got %+v", status)
	}
	if _, err := c.SetNotify(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	status, _ = c.Status(context.Background())
	if !status.NotifyEnabled {
		t.Fatalf("Status should reflect notify-on, got %+v", status)
	}
}

// SetNotify must not flip the in-memory preference when the disk write fails.
func TestUpdateController_SetNotifyWriteFailureKeepsMemory(t *testing.T) {
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		return update.Result{Available: false, Current: current, Latest: current}, nil
	})
	c.persistNotify = func(bool) error { return errors.New("read-only fs") }
	if _, err := c.SetNotify(context.Background(), false); err == nil {
		t.Fatal("expected SetNotify to surface the write error")
	}
	if !c.notifyEnabled {
		t.Fatal("notifyEnabled should stay ON when the persist write fails")
	}
}

// checkAndLog logs each newly-available version once (headless-agent
// breadcrumb) and dedupes repeats of the same version.
func TestUpdateController_CheckAndLogDedupes(t *testing.T) {
	latest := "1.4.0"
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		return update.Result{Available: latest != current, Current: current, Latest: latest}, nil
	})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c.checkAndLog(context.Background(), logger)
	c.checkAndLog(context.Background(), logger) // same version cached → no extra log
	if n := strings.Count(buf.String(), "waired update available"); n != 1 {
		t.Fatalf("expected exactly one log for v1.4.0, got %d:\n%s", n, buf.String())
	}

	// A newer version logs again (force a fresh check past the cache).
	latest = "1.5.0"
	c.lastLoggedVersion = "1.4.0"
	c.hasResult = false // drop the cache so Check re-resolves
	c.checkAndLog(context.Background(), logger)
	if n := strings.Count(buf.String(), "waired update available"); n != 2 {
		t.Fatalf("expected a second log for v1.5.0, got %d:\n%s", n, buf.String())
	}
}

// When no update is available, checkAndLog stays quiet.
func TestUpdateController_CheckAndLogQuietWhenIdle(t *testing.T) {
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		return update.Result{Available: false, Current: current, Latest: current}, nil
	})
	var buf bytes.Buffer
	c.checkAndLog(context.Background(), slog.New(slog.NewTextHandler(&buf, nil)))
	if strings.Contains(buf.String(), "waired update available") {
		t.Fatalf("should not log when up to date:\n%s", buf.String())
	}
}

// runUpdateCheckLoop must return immediately (never resolving the feed) when
// the context is already cancelled, and tolerate nil / non-positive inputs.
func TestRunUpdateCheckLoop_CancelledAndGuards(t *testing.T) {
	var calls int32
	c, _ := newTestController(func(_ context.Context, current string) (update.Result, error) {
		atomic.AddInt32(&calls, 1)
		return update.Result{Available: true, Current: current, Latest: "1.4.0"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runUpdateCheckLoop(ctx, c, updateCheckInterval, nil) // returns at the first ctx.Done select
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("cancelled ctx must skip the initial check, got %d calls", n)
	}
	// Guards: nil controller and non-positive interval are no-ops.
	runUpdateCheckLoop(context.Background(), nil, updateCheckInterval, nil)
	runUpdateCheckLoop(context.Background(), c, 0, nil)
}
