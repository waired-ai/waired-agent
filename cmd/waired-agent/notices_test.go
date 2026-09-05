package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
)

func lighterRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		Direction:     management.RecommendationLighter,
		FromModelID:   "heavy",
		FromVariantID: "q4",
		ToModelID:     "light",
		ToVariantID:   "q4",
		MeasuredTokps: 42,
		FloorTokps:    60,
	}
}

func upgradeRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		Direction:      management.RecommendationUpgrade,
		FromModelID:    "light",
		FromVariantID:  "q4",
		ToModelID:      "heavy",
		ToVariantID:    "q4",
		MeasuredTokps:  118,
		PredictedTokps: 64,
	}
}

// TestNoticesFromRecommendations_DismissedSaysNothing
//
// PRODUCT CONTRACT (management.BenchmarkRecommendation.Dismissed is
// published "so the CLI/tray can stay silent" about a pairing the person
// has already declined). A notice is the third surface reading that
// field, and the quietest of the three to get wrong: unlike a prompt, it
// persists.
func TestNoticesFromRecommendations_DismissedSaysNothing(t *testing.T) {
	rec := lighterRec()
	rec.Dismissed = true
	if got := noticesFromRecommendations(rec, nil); len(got) != 0 {
		t.Fatalf("got %d notices for a dismissed suggestion, want none", len(got))
	}

	up := upgradeRec()
	up.Dismissed = true
	if got := noticesFromRecommendations(nil, up); len(got) != 0 {
		t.Fatalf("got %d notices for a dismissed upgrade, want none", len(got))
	}
}

// TestNoticesFromRecommendations_CarriesBothDirections
//
// PRODUCT CONTRACT (waired-agent#1205, which names the step-up
// suggestion as having the same delivery hole as the step-down one).
// Severity is the load-bearing difference: `waired doctor` shows the
// warning and not the suggestion, because a better model being available
// is not a fault in the setup.
func TestNoticesFromRecommendations_CarriesBothDirections(t *testing.T) {
	got := noticesFromRecommendations(lighterRec(), nil)
	if len(got) != 1 || got[0].Kind != notice.KindLighterModel {
		t.Fatalf("lighter: got %+v", got)
	}
	if got[0].Severity != notice.SeverityWarn || got[0].Target != "light" {
		t.Errorf("lighter: %+v", got[0])
	}

	got = noticesFromRecommendations(nil, upgradeRec())
	if len(got) != 1 || got[0].Kind != notice.KindBetterModel {
		t.Fatalf("upgrade: got %+v", got)
	}
	if got[0].Severity != notice.SeverityInfo {
		t.Errorf("a step-up suggestion is not a problem: %+v", got[0])
	}
}

// TestNoticesFromRecommendations_LighterWins records today's behaviour.
// The daemon makes the two mutually exclusive; should that ever slip,
// the surfaces already prefer the step-down, and this keeps the notice
// field agreeing with them.
func TestNoticesFromRecommendations_LighterWins(t *testing.T) {
	got := noticesFromRecommendations(lighterRec(), upgradeRec())
	if len(got) != 1 || got[0].Kind != notice.KindLighterModel {
		t.Fatalf("got %+v, want only the step-down", got)
	}
}

// TestNoticesFromRecommendations_NoTargetSaysNothing records today's
// behaviour: a suggestion that cannot name a model to switch to has
// nothing a person could act on.
func TestNoticesFromRecommendations_NoTargetSaysNothing(t *testing.T) {
	rec := lighterRec()
	rec.ToModelID = ""
	if got := noticesFromRecommendations(rec, nil); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

// noticeTestProvider is a provider wired for the real derivation path:
// a store with heavy/q4 active, the two-family fixture catalogue, and a
// profiler whose RAM and engine version are pinned so the answer does
// not depend on the machine running the test.
func noticeTestProvider(t *testing.T, reg *notice.Registry) *agentInferenceProvider {
	t.Helper()
	return &agentInferenceProvider{
		cfg:       agentconfig.InferenceConfig{},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:     storeWithActive(t),
		manifests: recTestManifests(),
		profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 16, 12, nil }),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return nil, hardware.Accelerators{}, nil
			}),
			hardware.WithEngineVersion(func(_ context.Context, name string) (bool, string) {
				return name == "ollama", "0.31.0"
			}),
		),
		notices: reg,
	}
}

// TestPublishRecommendationNotices_PublishesWhatTheBenchmarkSays
//
// PRODUCT CONTRACT (waired-agent#1205). This is the end of the wire the
// issue is about: a host that measured below its floor has to end up
// saying so somewhere a person can read it.
func TestPublishRecommendationNotices_PublishesWhatTheBenchmarkSays(t *testing.T) {
	reg := notice.NewRegistry(time.Minute, nil)
	p := noticeTestProvider(t, reg)
	p.SetLastBench(BenchResult{TokensPerSec: 10, Capacity: 1, ModelID: "heavy"})

	p.publishRecommendationNotices(context.Background())

	got := reg.Active()
	if len(got) != 1 || got[0].Kind != notice.KindLighterModel {
		t.Fatalf("got %+v, want one step-down suggestion", got)
	}
	if got[0].Target != "light" {
		t.Errorf("target = %q, want light", got[0].Target)
	}
}

// TestPublishRecommendationNotices_ClearsWhenTheConditionGoesAway
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: a notice disappears when
// it stops being produced). Republishing derives afresh, so a host that
// clears its floor stops saying it is below one — nothing has to notice
// and clear the old message.
func TestPublishRecommendationNotices_ClearsWhenTheConditionGoesAway(t *testing.T) {
	reg := notice.NewRegistry(time.Minute, nil)
	p := noticeTestProvider(t, reg)
	p.SetLastBench(BenchResult{TokensPerSec: 10, Capacity: 1, ModelID: "heavy"})
	p.publishRecommendationNotices(context.Background())
	if len(reg.Active()) != 1 {
		t.Fatalf("setup: expected the suggestion to be published first")
	}

	p.SetLastBench(BenchResult{TokensPerSec: 500, Capacity: 1, ModelID: "heavy"})
	p.publishRecommendationNotices(context.Background())

	if got := reg.Active(); len(got) != 0 {
		t.Fatalf("got %+v, want none once the host clears its floor", got)
	}
}

// TestPublishRecommendationNotices_WithoutARegistryDoesNothing records
// today's behaviour: every unit test that builds a provider directly
// leaves the registry nil, and none of them should have to care.
func TestPublishRecommendationNotices_WithoutARegistryDoesNothing(t *testing.T) {
	p := noticeTestProvider(t, nil)
	p.SetLastBench(BenchResult{TokensPerSec: 10, Capacity: 1, ModelID: "heavy"})
	p.publishRecommendationNotices(context.Background())
}

// TestRunNoticeLoop_PublishesBeforeTheFirstTick records today's
// behaviour. A daemon that has just restarted with a standing condition
// should say so at once; waiting a heartbeat would make a restart look
// like the condition had cleared.
func TestRunNoticeLoop_PublishesBeforeTheFirstTick(t *testing.T) {
	published := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An hour between ticks: anything this test observes came from the
	// publish before the ticker, not from a tick.
	go runNoticeLoop(ctx, time.Hour, func(context.Context) {
		select {
		case published <- struct{}{}:
		default:
		}
	})

	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop published nothing before its first tick")
	}
}

// TestRunNoticeLoop_KeepsRepublishing records today's behaviour: the
// lease only holds while a producer keeps repeating itself.
func TestRunNoticeLoop_KeepsRepublishing(t *testing.T) {
	calls := make(chan struct{}, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runNoticeLoop(ctx, time.Millisecond, func(context.Context) {
		select {
		case calls <- struct{}{}:
		default:
		}
	})

	deadline := time.After(5 * time.Second)
	for range 3 {
		select {
		case <-calls:
		case <-deadline:
			t.Fatal("the loop stopped republishing")
		}
	}
}

// TestRunNoticeLoop_StopsWithItsContext records today's behaviour: the
// loop is one of the daemon's long-lived goroutines and must not outlive
// shutdown.
func TestRunNoticeLoop_StopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runNoticeLoop(ctx, 10*time.Millisecond, func(context.Context) {})
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop outlived its context")
	}
}

// TestNoticeProviderIsNilSafe records today's behaviour: the management
// route is wired before anything has published, and an unconfigured
// registry answers with nothing rather than panicking in a handler.
func TestNoticeProviderIsNilSafe(t *testing.T) {
	if got := (noticeProvider{}).Notices(); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
