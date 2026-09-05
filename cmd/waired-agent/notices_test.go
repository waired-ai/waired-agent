package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

// TestUpdateNotices_SaysNothingWithoutARelease
//
// PRODUCT CONTRACT (#1229). A producer publishes its whole set every
// time, so "there is no update" has to be expressible — and it is what
// makes the row go when a host is brought up to date.
func TestUpdateNotices_SaysNothingWithoutARelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   management.UpdateStatus
	}{
		{"up to date", management.UpdateStatus{Phase: management.UpdatePhaseIdle, CurrentVersion: "v0.9.3"}},
		{"available but unnamed", management.UpdateStatus{Available: true, CurrentVersion: "v0.9.1"}},
		{"check failed", management.UpdateStatus{Phase: management.UpdatePhaseError, Error: "dial tcp: no route to host"}},
		{"never checked", management.UpdateStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateNotices(tc.st); len(got) != 0 {
				t.Errorf("updateNotices = %+v, want nothing", got)
			}
		})
	}
}

// TestUpdateNotices_CarriesBothVersions
//
// PRODUCT CONTRACT (#1229): `waired status` never mentioned an available
// update before this — only `waired update` read the verdict — so the
// notice has to carry enough to act on without running that command.
func TestUpdateNotices_CarriesBothVersions(t *testing.T) {
	got := updateNotices(management.UpdateStatus{
		Phase:          management.UpdatePhaseAvailable,
		Available:      true,
		CurrentVersion: "v0.9.1",
		LatestVersion:  "v0.9.3",
		ApplyMethod:    "apt",
	})
	if len(got) != 1 {
		t.Fatalf("updateNotices = %+v, want one", got)
	}
	n := got[0]
	if n.Kind != notice.KindUpdateAvailable || n.Severity != notice.SeverityInfo {
		t.Errorf("notice = %+v, want an info update notice", n)
	}
	if !strings.Contains(n.Title, "v0.9.3") || !strings.Contains(n.Text, "v0.9.1") {
		t.Errorf("notice = %q / %q, want both versions", n.Title, n.Text)
	}
	if n.Action != notice.ActionInstallUpdate {
		t.Errorf("action = %v, want the install action the tray banner carried", n.Action)
	}
}

// TestUpdateNoticePublisher_KeepsToItsOwnSource
//
// PRODUCT CONTRACT (decision record 20260905/0000, rule 1: a later
// producer never overwrites an earlier one). This is the first PR where
// more than one producer exists, so it is the first one where that rule
// could be broken.
func TestUpdateNoticePublisher_KeepsToItsOwnSource(t *testing.T) {
	reg := notice.NewRegistry(time.Minute, nil)
	reg.Publish(noticeSourceRecommendation, []notice.Notice{
		notice.LighterModel("qwen3-30b-a3b", "qwen3-8b-instruct", 13.8, 60),
	})

	uc := &updateController{current: "v0.9.1", now: time.Now}
	uc.hasResult = true
	uc.cached = management.UpdateStatus{
		Phase: management.UpdatePhaseAvailable, Available: true,
		CurrentVersion: "v0.9.1", LatestVersion: "v0.9.3",
	}

	pub := updateNoticePublisher(reg, uc)
	if pub == nil {
		t.Fatal("updateNoticePublisher returned nil with a registry and a controller")
	}
	pub(context.Background())

	active := reg.Active()
	if len(active) != 2 {
		t.Fatalf("Active = %+v, want the suggestion and the update", active)
	}
	kinds := map[notice.Kind]bool{}
	for _, n := range active {
		kinds[n.Kind] = true
	}
	if !kinds[notice.KindLighterModel] || !kinds[notice.KindUpdateAvailable] {
		t.Errorf("Active carries %v, want both producers", kinds)
	}
	// The warning sorts above the info notice: severity is the first key.
	if active[0].Kind != notice.KindLighterModel {
		t.Errorf("first row is %q, want the warning ahead of the info notice", active[0].Kind)
	}

	// And it stops saying it when the host is brought up to date, without
	// touching what the other producer said.
	uc.cached = management.UpdateStatus{Phase: management.UpdatePhaseIdle, CurrentVersion: "v0.9.3"}
	pub(context.Background())
	if active := reg.Active(); len(active) != 1 || active[0].Kind != notice.KindLighterModel {
		t.Errorf("Active = %+v, want only the suggestion", active)
	}
}

// TestUpdateNoticePublisher_NilIsNotALoop records today's behaviour:
// runNoticeLoop returns at once on a nil republisher, so a build without
// an update controller starts no ticker rather than panicking in one.
func TestUpdateNoticePublisher_NilIsNotALoop(t *testing.T) {
	if pub := updateNoticePublisher(nil, &updateController{}); pub != nil {
		t.Error("no registry, want no publisher")
	}
	if pub := updateNoticePublisher(notice.NewRegistry(0, nil), nil); pub != nil {
		t.Error("no controller, want no publisher")
	}
}

// TestEngineNotices_BothWarningsAreSaid
//
// PRODUCT CONTRACT (#1229). This is the defect: `waired doctor` returns
// one finding per engine and reached the tuning warning only when there
// was no version warning, so a host with both was told about one of them
// and never learned about the other. A list cannot do that.
func TestEngineNotices_BothWarningsAreSaid(t *testing.T) {
	got := engineNotices(engineProvenance{
		Engine:         "ollama",
		VersionWarning: "engine version 0.24.0 does not match the bundled pin 0.33.2",
		TuningWarning:  "model spills to system RAM even at the minimum context window on this host",
		TuningDegraded: true,
	}, true, true, true)
	if len(got) != 2 {
		t.Fatalf("engineNotices = %+v, want both the version and the tuning notice", got)
	}
	kinds := map[notice.Kind]notice.Notice{}
	for _, n := range got {
		kinds[n.Kind] = n
	}
	v, okV := kinds[notice.KindEngineVersion]
	tn, okT := kinds[notice.KindEngineTuning]
	if !okV || !okT {
		t.Fatalf("engineNotices = %+v, want one of each kind", got)
	}
	if !strings.Contains(v.Text, "0.24.0") || !strings.Contains(tn.Text, "spills") {
		t.Errorf("details did not survive: %q / %q", v.Text, tn.Text)
	}
	if v.Severity != notice.SeverityWarn || tn.Severity != notice.SeverityWarn {
		t.Errorf("severities = %v / %v, want both warn on a degraded host", v.Severity, tn.Severity)
	}
}

// TestEngineNotices_ADeliberateTradeIsNotAWarning
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: a notice is a warning only
// for something Waired can assert is unwanted). The same field carries a
// window traded against decode speed on purpose; `waired doctor` warned
// about that host because it keyed on the string being non-empty.
func TestEngineNotices_ADeliberateTradeIsNotAWarning(t *testing.T) {
	got := engineNotices(engineProvenance{
		Engine:        "ollama",
		TuningWarning: "context window set to 200000 tokens for coding-agent workloads; about 12% of the model is expected to sit in system RAM (larger window traded for some decode speed)",
	}, true, true, true)
	if len(got) != 1 {
		t.Fatalf("engineNotices = %+v, want the tuning note", got)
	}
	if got[0].Severity != notice.SeverityInfo {
		t.Errorf("severity = %v, want info — this host is doing what it was asked", got[0].Severity)
	}
}

// TestEngineNotices_SaysNothingWhenThereIsNothingToSay
//
// PRODUCT CONTRACT (#1229): a producer publishes its whole set, so an
// engine with no complaint has to be expressible — it is what makes the
// rows go when a pin is brought back into line.
func TestEngineNotices_SaysNothingWhenThereIsNothingToSay(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   engineProvenance
	}{
		{"healthy", engineProvenance{Engine: "ollama", Version: "0.33.3"}},
		{"no subsystem to ask", engineProvenance{}},
		{"a stopped engine is state, not advice", engineProvenance{
			Engine: "vllm", FailureReason: "the engine could not bind 127.0.0.1:9479"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineNotices(tc.in, true, true, true); len(got) != 0 {
				t.Errorf("engineNotices = %+v, want nothing", got)
			}
		})
	}
}

// TestEngineNoticePublisher_ReplacesItsOwnSet
//
// PRODUCT CONTRACT (decision record 20260905/0000, rule 3: the publish
// unit is a producer's whole set). A version warning that is fixed while
// a tuning note stands must leave one row, not two.
func TestEngineNoticePublisher_ReplacesItsOwnSet(t *testing.T) {
	reg := notice.NewRegistry(time.Minute, nil)
	prov := engineProvenance{
		Engine:         "ollama",
		VersionWarning: "engine version 0.24.0 does not match the bundled pin 0.33.2",
		TuningWarning:  "context window kept at 200000 though host memory fits ~120000 tokens un-spilled",
	}
	pub := engineNoticePublisher(reg, func() engineProvenance { return prov }, nil)
	if pub == nil {
		t.Fatal("engineNoticePublisher returned nil with a registry and an accessor")
	}
	pub(context.Background())
	if got := reg.Active(); len(got) != 2 {
		t.Fatalf("Active = %+v, want two", got)
	}

	prov.VersionWarning = ""
	pub(context.Background())
	got := reg.Active()
	if len(got) != 1 || got[0].Kind != notice.KindEngineTuning {
		t.Fatalf("Active = %+v, want only the tuning note", got)
	}
}
