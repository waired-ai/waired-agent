package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// All identifiers here are obviously synthetic: waired-agent is a public
// repo and real device / grant ids must never appear in fixtures.
const (
	usageTestDeviceID = "dev_test00000001"
	usageTestGrantID  = "grant_test0001"
)

type fakeUsageAPI struct {
	mu      sync.Mutex
	reports []signer.PublicUsageReport
	errs    []error // consumed in order; nil (or exhausted) means success
}

func (f *fakeUsageAPI) PushPublicUsage(_ context.Context, _ string, r signer.PublicUsageReport, _ ed25519.PrivateKey) (controlclient.PublicUsagePushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	if err != nil {
		return controlclient.PublicUsagePushResult{}, err
	}
	return controlclient.PublicUsagePushResult{Status: "ok", Inserted: len(r.Entries)}, nil
}

func (f *fakeUsageAPI) snapshot() []signer.PublicUsageReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]signer.PublicUsageReport(nil), f.reports...)
}

func usageLoopDeps(api publicUsageAPI, batch *publicUsageBatch) publicUsageDeps {
	return publicUsageDeps{
		API:             api,
		Batch:           batch,
		DeviceID:        usageTestDeviceID,
		MachineKey:      make(ed25519.PrivateKey, ed25519.PrivateKeySize),
		Logger:          quietLogger(),
		Interval:        5 * time.Millisecond,
		MinPushInterval: time.Millisecond,
	}
}

func TestPublicUsageBatch_AggregatesByTriple(t *testing.T) {
	b := newPublicUsageBatch(nil)
	b.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 10, 5, 100)
	b.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 3, 2, 50)
	b.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "sub", 1, 1, 10)
	b.Record("grant_test0002", "qwen3:8b-q4_K_M", "main", 7, 7, 70)

	entries, dropped := b.drain()
	if dropped != 0 {
		t.Fatalf("dropped = %d", dropped)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 distinct (grant, model, class) triples", len(entries))
	}
	var main *signer.PublicUsageEntry
	for i := range entries {
		if entries[i].GrantID == usageTestGrantID && entries[i].Class == "main" {
			main = &entries[i]
		}
	}
	if main == nil {
		t.Fatal("main-class entry missing")
	}
	if main.Requests != 2 || main.InputTokens != 13 || main.OutputTokens != 7 || main.InferenceMS != 150 {
		t.Fatalf("aggregate = %+v", *main)
	}
	if main.WindowStart == "" || main.WindowEnd == "" {
		t.Errorf("window not stamped: %+v", *main)
	}

	// drain empties the accumulator.
	if again, _ := b.drain(); len(again) != 0 {
		t.Fatalf("drain left %d entries behind", len(again))
	}
}

func TestPublicUsageBatch_IgnoresIncompleteSamples(t *testing.T) {
	b := newPublicUsageBatch(nil)
	b.Record("", "qwen3:8b-q4_K_M", "main", 1, 1, 1)
	b.Record(usageTestGrantID, "", "main", 1, 1, 1)
	if entries, _ := b.drain(); len(entries) != 0 {
		t.Fatalf("recorded %d entries without a grant or model", len(entries))
	}
}

func TestPublicUsageBatch_BoundsMemory(t *testing.T) {
	b := newPublicUsageBatch(nil)
	for i := 0; i < publicUsageMaxKeys+25; i++ {
		b.Record(usageTestGrantID, "model", string(rune('a'+i%26))+string(rune('a'+i/26)), 1, 1, 1)
	}
	entries, dropped := b.drain()
	if len(entries) > publicUsageMaxKeys {
		t.Fatalf("accumulator grew to %d keys, cap is %d", len(entries), publicUsageMaxKeys)
	}
	if dropped == 0 {
		t.Fatal("dropped nothing but the cap was exceeded — the drop must be visible")
	}
}

func TestPublicUsageLoop_FlushesPeriodically(t *testing.T) {
	api := &fakeUsageAPI{}
	batch := newPublicUsageBatch(nil)
	batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 10, 5, 100)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicUsageLoop(ctx, usageLoopDeps(api, batch))

	usageWaitFor(t, 2*time.Second, func() bool { return len(api.snapshot()) > 0 })
	got := api.snapshot()[0]
	if len(got.Entries) != 1 || got.Entries[0].GrantID != usageTestGrantID {
		t.Fatalf("report = %+v", got)
	}
}

// A rate-limited flush is provably pre-insert, so it must be retried —
// under-reporting a user's served work is a real loss.
func TestPublicUsageLoop_RequeuesRetryableRejection(t *testing.T) {
	api := &fakeUsageAPI{errs: []error{
		&controlclient.PublicUsageError{Status: http.StatusTooManyRequests, Code: "rate_limited"},
	}}
	batch := newPublicUsageBatch(nil)
	batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 10, 5, 100)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicUsageLoop(ctx, usageLoopDeps(api, batch))

	usageWaitFor(t, 2*time.Second, func() bool { return len(api.snapshot()) >= 2 })
	second := api.snapshot()[1]
	if len(second.Entries) != 1 || second.Entries[0].Requests != 1 {
		t.Fatalf("retry lost the entry: %+v", second)
	}
}

// A 5xx may have landed AFTER the ledger write, and the endpoint is not
// idempotent, so the chunk is dropped: under-counting beats
// double-counting.
func TestPublicUsageLoop_DropsAmbiguousFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"server error", &controlclient.PublicUsageError{Status: http.StatusInternalServerError, Code: "internal_error"}},
		{"transport failure", errors.New("dial tcp: connection refused")},
		{"permanent rejection", &controlclient.PublicUsageError{Status: http.StatusForbidden, Code: "grant_not_owned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeUsageAPI{errs: []error{tc.err}}
			batch := newPublicUsageBatch(nil)
			batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 10, 5, 100)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go runPublicUsageLoop(ctx, usageLoopDeps(api, batch))

			usageWaitFor(t, 2*time.Second, func() bool { return len(api.snapshot()) > 0 })
			// Give the loop several more ticks; nothing may be re-sent.
			time.Sleep(60 * time.Millisecond)
			for i, r := range api.snapshot() {
				if i == 0 {
					continue
				}
				if len(r.Entries) > 0 {
					t.Fatalf("re-sent a chunk whose fate is unknown: %+v", r)
				}
			}
		})
	}
}

func TestPublicUsageLoop_FlushesOnShutdown(t *testing.T) {
	api := &fakeUsageAPI{}
	batch := newPublicUsageBatch(nil)
	batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 10, 5, 100)

	deps := usageLoopDeps(api, batch)
	deps.Interval = time.Hour // the periodic path cannot fire

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runPublicUsageLoop(ctx, deps) }()
	cancel()
	<-done

	if len(api.snapshot()) != 1 {
		t.Fatalf("shutdown flush did not report: %d pushes", len(api.snapshot()))
	}
}

// Record is on the :9474 request path. It must never wait on the CP, so
// the batch mutex is never held across a push.
func TestPublicUsageBatch_RecordNeverBlocksOnThePush(t *testing.T) {
	release := make(chan struct{})
	api := &blockingUsageAPI{release: release}
	batch := newPublicUsageBatch(nil)
	batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "main", 1, 1, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicUsageLoop(ctx, usageLoopDeps(api, batch))

	usageWaitFor(t, 2*time.Second, func() bool { return api.inFlight() })

	recorded := make(chan struct{})
	go func() {
		batch.Record(usageTestGrantID, "qwen3:8b-q4_K_M", "sub", 1, 1, 1)
		close(recorded)
	}()
	select {
	case <-recorded:
	case <-time.After(time.Second):
		t.Fatal("Record blocked while a push was in flight")
	}
	close(release)
}

type blockingUsageAPI struct {
	mu      sync.Mutex
	started bool
	release chan struct{}
}

func (b *blockingUsageAPI) PushPublicUsage(ctx context.Context, _ string, _ signer.PublicUsageReport, _ ed25519.PrivateKey) (controlclient.PublicUsagePushResult, error) {
	b.mu.Lock()
	b.started = true
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return controlclient.PublicUsagePushResult{}, nil
}

func (b *blockingUsageAPI) inFlight() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started
}

// --- sink ---

func TestPublicUsageSink_OnlyReportsPublicGuests(t *testing.T) {
	batch := newPublicUsageBatch(nil)
	sink := publicUsageSink(batch)

	sample := gateway.UsageSample{
		ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M",
		Class: "main", InputTokens: 10, OutputTokens: 5, DurationMS: 100,
		Status: http.StatusOK,
	}

	t.Run("no peer identity at all", func(t *testing.T) {
		sink(context.Background(), sample)
		if e, _ := batch.drain(); len(e) != 0 {
			t.Fatalf("reported a request with no peer context: %+v", e)
		}
	})

	t.Run("own-account mesh peer", func(t *testing.T) {
		ctx := inference.ContextWithPeer(context.Background(), inference.PeerIdentity{
			DeviceID: "dev_own00000001",
		})
		sink(ctx, sample)
		if e, _ := batch.drain(); len(e) != 0 {
			t.Fatalf("reported an own-account peer's usage: %+v", e)
		}
	})

	t.Run("public guest", func(t *testing.T) {
		ctx := inference.ContextWithPeer(context.Background(), inference.PeerIdentity{
			DeviceID: "dev_guest0000001",
			Grant:    &signer.PeerGrant{ID: usageTestGrantID, Kind: "public", Role: "consumer"},
		})
		sink(ctx, sample)
		entries, _ := batch.drain()
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want 1", len(entries))
		}
		if entries[0].GrantID != usageTestGrantID {
			t.Errorf("GrantID = %q", entries[0].GrantID)
		}
		// The CP resolves the quality tier from the ENGINE name.
		if entries[0].ModelID != "qwen3:8b-q4_K_M" {
			t.Errorf("ModelID = %q, want the engine-native name", entries[0].ModelID)
		}
	})
}

func TestPublicUsageSink_NilBatch(t *testing.T) {
	if publicUsageSink(nil) != nil {
		t.Fatal("a nil batch must yield a nil sink so the gateway skips emission")
	}
}

// usageWaitFor polls until cond holds or the deadline passes. Named
// distinctly: waitFor and grantWaitFor are already taken in this package.
func usageWaitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
