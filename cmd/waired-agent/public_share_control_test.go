package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// fakePusher records PushPublicShare calls and returns a scripted
// result/error sequence (last entry repeats).
type fakePusher struct {
	mu    sync.Mutex
	calls []struct {
		enabled    bool
		maxClients int
	}
	errs   []error
	result controlclient.PublicSharePushResult
}

func (f *fakePusher) push(_ context.Context, enabled bool, maxClients int) (controlclient.PublicSharePushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		enabled    bool
		maxClients int
	}{enabled, maxClients})
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		if len(f.errs) > 1 {
			f.errs = f.errs[1:]
		}
	}
	if err != nil {
		return controlclient.PublicSharePushResult{}, err
	}
	res := f.result
	res.Enabled = enabled
	return res, nil
}

func (f *fakePusher) lastCall() (enabled bool, maxClients int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls[len(f.calls)-1]
	return c.enabled, c.maxClients
}

// TestPublicShareController_DefaultOff pins the opt-in contract: with
// no persisted choice (empty initial) the controller boots OFF and the
// gate-facing IsPublicShareDenied reads true.
func TestPublicShareController_DefaultOff(t *testing.T) {
	pc := newPublicShareController(t.TempDir(), "", nil)
	if pc.IsPublic() || !pc.IsPublicShareDenied() {
		t.Fatalf("default state: IsPublic=%v IsPublicShareDenied=%v, want false/true", pc.IsPublic(), pc.IsPublicShareDenied())
	}
	current, desired := pc.State()
	if current != state.PublicShareOff || desired != state.PublicShareOff {
		t.Fatalf("State() = (%q, %q), want both %q", current, desired, state.PublicShareOff)
	}
	if !pc.Synced() {
		t.Fatal("Synced() = false with no pending push")
	}
}

// TestPublicShareController_EnablePersistsAndReboots: Enable flips the
// live flag and persists, so a controller rebuilt from the same state
// dir boots ON. With no pusher wired the result reports CPSynced=false
// without going pending (nothing to retry).
func TestPublicShareController_EnablePersistsAndReboots(t *testing.T) {
	dir := t.TempDir()
	pc := newPublicShareController(dir, "", nil)
	res, err := pc.Enable(context.Background(), 0)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if res.CPSynced {
		t.Fatal("CPSynced = true with no pusher wired")
	}
	if !pc.Synced() {
		t.Fatal("Synced() = false: a pusher-less transition must not go pending")
	}
	if !pc.IsPublic() {
		t.Fatal("IsPublic after Enable = false")
	}
	persisted, err := state.ReadDesiredPublicShare(dir)
	if err != nil || persisted != state.PublicShareOn {
		t.Fatalf("persisted = (%q, %v), want %q", persisted, err, state.PublicShareOn)
	}
	reboot := newPublicShareController(dir, persisted, nil)
	if !reboot.IsPublic() {
		t.Fatal("rebooted controller: IsPublic = false, want true")
	}
}

// TestPublicShareController_DisableFiresKillSwitch: the OFF transition
// runs the registered onDisable hook (wired to AbortPublicInFlight)
// and flips the deny flag BEFORE the CP push happens — §8.3 step 1
// never waits on the network.
func TestPublicShareController_DisableFiresKillSwitch(t *testing.T) {
	pc := newPublicShareController(t.TempDir(), state.PublicShareOn, nil)
	fired := 0
	pushed := 0
	pc.SetOnDisable(func() {
		fired++
		if !pc.IsPublicShareDenied() {
			t.Error("onDisable ran before the deny flag flipped")
		}
		if pushed != 0 {
			t.Error("onDisable ran after the CP push; local abort must come first")
		}
	})
	pc.SetPusher(func(context.Context, bool, int) (controlclient.PublicSharePushResult, error) {
		pushed++
		return controlclient.PublicSharePushResult{RevokedGrants: 2}, nil
	})
	res, err := pc.Disable(context.Background())
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if fired != 1 {
		t.Fatalf("onDisable fired %d times, want 1", fired)
	}
	if !res.CPSynced || res.RevokedGrants != 2 {
		t.Fatalf("result = %+v, want CPSynced=true RevokedGrants=2", res)
	}
	// Enable does not fire the hook.
	if _, err := pc.Enable(context.Background(), 0); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if fired != 1 {
		t.Fatalf("onDisable fired %d times after Enable, want still 1", fired)
	}
}

// TestPublicShareController_EnablePushesAndForwardsMaxClients: the
// operator's max_clients rides the push verbatim and the CP echo comes
// back in the result.
func TestPublicShareController_EnablePushesAndForwardsMaxClients(t *testing.T) {
	fp := &fakePusher{result: controlclient.PublicSharePushResult{MaxClients: 2}}
	pc := newPublicShareController(t.TempDir(), "", nil)
	pc.SetPusher(fp.push)
	res, err := pc.Enable(context.Background(), 3)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !res.CPSynced || res.MaxClients != 2 {
		t.Fatalf("result = %+v, want CPSynced=true MaxClients=2", res)
	}
	enabled, maxClients := fp.lastCall()
	if !enabled || maxClients != 3 {
		t.Fatalf("push saw (enabled=%v, max_clients=%d), want (true, 3)", enabled, maxClients)
	}
}

// TestPublicShareController_MeshAutoEnableOrderingAndAbort: enabling
// public share first enables mesh share; a mesh failure aborts the
// public enable entirely (fail loudly — an unmatched public node is
// worse than an error).
func TestPublicShareController_MeshAutoEnableOrderingAndAbort(t *testing.T) {
	dir := t.TempDir()
	pc := newPublicShareController(dir, "", nil)
	meshErr := errors.New("mesh persist failed")
	pc.SetMeshAutoEnable(func(context.Context) (bool, error) { return false, meshErr })
	if _, err := pc.Enable(context.Background(), 0); !errors.Is(err, meshErr) {
		t.Fatalf("Enable error = %v, want wrapped %v", err, meshErr)
	}
	if pc.IsPublic() {
		t.Fatal("public flag flipped despite mesh auto-enable failure")
	}
	if persisted, _ := state.ReadDesiredPublicShare(dir); persisted == state.PublicShareOn {
		t.Fatal("desired state persisted ON despite mesh auto-enable failure")
	}

	pc.SetMeshAutoEnable(func(context.Context) (bool, error) { return true, nil })
	res, err := pc.Enable(context.Background(), 0)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !res.MeshShareEnabled {
		t.Fatal("MeshShareEnabled = false, want true when the hook reports a change")
	}
}

// TestPublicShareController_PushFailureGoesPendingAndRecovers: a failed
// synchronous push leaves the transition applied locally, flags
// pending, and RunSync retries until the CP acknowledges.
func TestPublicShareController_PushFailureGoesPendingAndRecovers(t *testing.T) {
	fp := &fakePusher{errs: []error{errors.New("cp down"), errors.New("cp down"), nil}}
	pc := newPublicShareController(t.TempDir(), "", nil)
	pc.retryMin, pc.retryMax = 5*time.Millisecond, 20*time.Millisecond
	pc.SetPusher(fp.push)

	res, err := pc.Enable(context.Background(), 1)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if res.CPSynced {
		t.Fatal("CPSynced = true despite push failure")
	}
	if !pc.IsPublic() {
		t.Fatal("local transition must survive a push failure")
	}
	if pc.Synced() {
		t.Fatal("Synced() = true, want pending")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pc.RunSync(ctx); close(done) }()

	deadline := time.Now().Add(30 * time.Second)
	for pc.pushPending.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pc.pushPending.Load() {
		t.Fatal("RunSync never cleared the pending push")
	}
	enabled, maxClients := fp.lastCall()
	if !enabled || maxClients != 1 {
		t.Fatalf("retry pushed (enabled=%v, max_clients=%d), want (true, 1)", enabled, maxClients)
	}
	cancel()
	<-done
}

// TestPublicShareController_ReconcileRemote covers the netmap echo
// adoption rules: adopt ON always; adopt OFF only after the CP has
// demonstrably folded the field (echoTrueSeen — a pre-B2 CP reports
// false unconditionally); no re-fire on unchanged state; suppression
// while a local transition is pending inside the window.
func TestPublicShareController_ReconcileRemote(t *testing.T) {
	pc := newPublicShareController(t.TempDir(), "", nil)
	kills := 0
	pc.SetOnDisable(func() { kills++ })

	// Pre-B2 guard: local ON, echo false, never seen true → keep ON.
	if _, err := pc.Enable(context.Background(), 0); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	pc.ReconcileRemote(false)
	if !pc.IsPublic() {
		t.Fatal("false echo adopted before the CP ever asserted true (pre-B2 guard)")
	}

	// CP asserts true (no change) → no transition, but the guard lifts.
	pc.ReconcileRemote(true)
	if !pc.IsPublic() || kills != 0 {
		t.Fatalf("true echo on ON state changed something: IsPublic=%v kills=%d", pc.IsPublic(), kills)
	}

	// Now a false echo is authoritative → adopt OFF, kill switch fires once.
	pc.ReconcileRemote(false)
	if pc.IsPublic() {
		t.Fatal("false echo not adopted after echoTrueSeen")
	}
	if kills != 1 {
		t.Fatalf("kill switch fired %d times, want 1", kills)
	}
	// Repeated false echo: no re-fire.
	pc.ReconcileRemote(false)
	if kills != 1 {
		t.Fatalf("kill switch re-fired on unchanged echo: %d", kills)
	}

	// Echo can adopt ON too.
	pc.ReconcileRemote(true)
	if !pc.IsPublic() {
		t.Fatal("true echo not adopted from OFF")
	}
}

// TestPublicShareController_PendingSuppressesEcho: while an
// unacknowledged local transition is inside the pending window the CP
// echo must not revert it; past the window the CP wins again.
func TestPublicShareController_PendingSuppressesEcho(t *testing.T) {
	fp := &fakePusher{errs: []error{errors.New("cp down")}}
	pc := newPublicShareController(t.TempDir(), "", nil)
	pc.SetPusher(fp.push)
	now := time.Now()
	pc.now = func() time.Time { return now }

	pc.echoTrueSeen.Store(true) // pretend B2 CP already proven
	if _, err := pc.Enable(context.Background(), 0); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if pc.Synced() {
		t.Fatal("want pending after failed push")
	}

	// Inside the window: stale false echo suppressed.
	pc.ReconcileRemote(false)
	if !pc.IsPublic() {
		t.Fatal("stale echo reverted a pending local enable inside the window")
	}

	// Past the window: the CP's echo wins again.
	now = now.Add(publicSharePendingWindow + time.Second)
	pc.ReconcileRemote(false)
	if pc.IsPublic() {
		t.Fatal("echo not adopted after the pending window expired")
	}
}
