package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// testOllamaAdapter builds an adapter pointed at a test server. The
// adapter derives its base URL from Host/Port, so the URL is split
// rather than assigned.
func testOllamaAdapter(t *testing.T, rawURL string, keepAlive time.Duration) *infruntime.OllamaAdapter {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Host: u.Hostname(), Port: port, KeepAlive: keepAlive,
	})
}

// TestResidencyFromPS pins the mapping from an /api/ps body onto the
// recorded observation. Record of today's behaviour, except where noted.
func TestResidencyFromPS(t *testing.T) {
	now := time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		body           psResponse
		wantResident   bool
		wantModel      string
		wantUntil      time.Time
		wantIndefinite bool
	}{
		{
			name: "nothing loaded is observed, not unknown",
			body: psResponse{},
		},
		{
			name:         "resident with a parseable expiry",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2026-08-20T02:30:00Z"}}},
			wantResident: true,
			wantModel:    "m:q4",
			wantUntil:    time.Date(2026, 8, 20, 2, 30, 0, 0, time.UTC),
		},
		{
			// An indefinite keep-alive is not a sentinel: Ollama renders
			// it as a date centuries out. Recording that as an expiry is
			// what made the product default print "until 2318-11-30"
			// (waired-agent#910), so it becomes a flag here and Until
			// stays zero — there is no deadline to report.
			name:           "indefinite is a flag, not a date",
			body:           psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2318-11-30T00:54:37Z"}}},
			wantResident:   true,
			wantModel:      "m:q4",
			wantIndefinite: true,
		},
		{
			// The horizon has to be far enough out that no setting an
			// operator could plausibly choose crosses it. A year is an
			// expiry.
			name:         "a year out is still an expiry",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2027-08-20T01:00:00Z"}}},
			wantResident: true,
			wantModel:    "m:q4",
			wantUntil:    time.Date(2027, 8, 20, 1, 0, 0, 0, time.UTC),
		},
		{
			// The weights are in memory whether or not we can read the
			// clock, so an unparseable expiry must still count as resident.
			name:         "unparseable expiry stays resident",
			body:         psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "not-a-time"}}},
			wantResident: true,
			wantModel:    "m:q4",
		},
		{
			name:         "missing expiry stays resident",
			body:         psResponse{Models: []psModel{{Name: "m:q4"}}},
			wantResident: true,
			wantModel:    "m:q4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := residencyFromPS(tc.body, now)
			if !got.Observed {
				t.Fatalf("Observed = false; a body we decoded is an observation")
			}
			if got.Resident() != tc.wantResident {
				t.Errorf("Resident() = %v, want %v", got.Resident(), tc.wantResident)
			}
			if got.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tc.wantModel)
			}
			if !got.Until.Equal(tc.wantUntil) {
				t.Errorf("Until = %v, want %v", got.Until, tc.wantUntil)
			}
			if got.Indefinite != tc.wantIndefinite {
				t.Errorf("Indefinite = %v, want %v", got.Indefinite, tc.wantIndefinite)
			}
			if !got.At.Equal(now) {
				t.Errorf("At = %v, want %v", got.At, now)
			}
		})
	}
}

// TestRefreshOllamaResidencyKeepsLastOnError pins that a probe failure
// leaves the previous observation alone. Rendering an unreachable engine
// as "not resident" would report an unload that did not happen — the
// same shape of wrong answer waired-agent#879 exists to remove.
func TestRefreshOllamaResidencyKeepsLastOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ad := testOllamaAdapter(t, srv.URL, 0)
	ad.SetResidency(infruntime.ModelResidency{Observed: true, Model: "m:q4"})
	refreshOllamaResidency(context.Background(), ad, srv.Client())

	if got := ad.Residency(); !got.Resident() || got.Model != "m:q4" {
		t.Errorf("a failed probe overwrote the last observation: %+v", got)
	}
}

// TestUnloadServingModel drives the release valve against a fake engine
// and asserts the request that actually frees the memory: keep_alive 0
// against the resident tag (waired-agent#861).
func TestUnloadServingModel(t *testing.T) {
	var loaded atomic.Bool
	loaded.Store(true)
	var gotKeepAlive, gotModel atomic.Value
	gotKeepAlive.Store("")
	gotModel.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			if loaded.Load() {
				_ = json.NewEncoder(w).Encode(psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2318-11-30T00:54:37Z"}}})
				return
			}
			_ = json.NewEncoder(w).Encode(psResponse{})
		case "/api/generate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ka, ok := body["keep_alive"].(string); ok {
				gotKeepAlive.Store(ka)
			}
			if m, ok := body["model"].(string); ok {
				gotModel.Store(m)
			}
			loaded.Store(false)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ad := testOllamaAdapter(t, srv.URL, 0)
	p := &agentInferenceProvider{ollama: ad}

	tag, err := p.UnloadServingModel(context.Background())
	if err != nil {
		t.Fatalf("UnloadServingModel: %v", err)
	}
	if tag != "m:q4" {
		t.Errorf("unloaded tag = %q, want m:q4", tag)
	}
	if got := gotKeepAlive.Load().(string); got != "0" {
		t.Errorf("keep_alive = %q, want \"0\" (unload after this request)", got)
	}
	if got := gotModel.Load().(string); got != "m:q4" {
		t.Errorf("model = %q, want m:q4", got)
	}
	// The surfaces must reflect the unload, not assume it.
	if res := ad.Residency(); !res.Observed || res.Resident() {
		t.Errorf("residency after unload = %+v, want observed and not resident", res)
	}

	// Unloading again is a success with an empty tag: the caller wanted
	// the memory back and the memory is back.
	tag, err = p.UnloadServingModel(context.Background())
	if err != nil {
		t.Fatalf("second UnloadServingModel: %v", err)
	}
	if tag != "" {
		t.Errorf("second unload returned %q, want empty", tag)
	}
}

// TestProviderKeepAliveFollowsConfig pins that the per-request value the
// warm path sends is the operator's setting, not a constant. Before
// waired-agent#861 this was a hardcoded 60m that no setting could move.
func TestProviderKeepAliveFollowsConfig(t *testing.T) {
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 20*time.Minute)}
	if got := p.keepAlive(); got != "20m0s" {
		t.Errorf("keepAlive() = %q, want 20m0s", got)
	}
	p = &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	// The default renders for the per-request field, so it is asserted as
	// a duration the engine can parse. The literal this used to pin
	// ("-1") is answered with a 400 by a live engine (waired-agent#927).
	def := p.keepAlive()
	d, err := time.ParseDuration(def)
	if err != nil {
		t.Errorf("keepAlive() with the default = %q, which the engine cannot parse: %v", def, err)
	} else if d >= 0 {
		t.Errorf("keepAlive() with the default = %q (%v), want a negative duration", def, d)
	}
}

// TestApplyResidency_ColdEngineRespawns pins the branch that
// waired-agent#908 is about: with nothing resident there is no model to
// re-stamp, and the setting only reaches a request-driven load through
// the process environment, which the engine read at spawn. So the
// managed case has to ASK FOR A RESPAWN, not quietly succeed.
func TestApplyResidency_ColdEngineRespawns(t *testing.T) {
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	// Pretend a reconcile is already running so requestEngineReconcile
	// records the intent and returns instead of spawning a goroutine that
	// would need a whole provider behind it.
	p.engineReconcileInFlight.Store(true)

	got := p.applyToColdEngine(infruntime.EngineModeSpawned)
	if got != management.ResidencyEffectEngineRestarted {
		t.Errorf("effect = %q, want %q", got, management.ResidencyEffectEngineRestarted)
	}
	if !p.engineRespawnPending.Load() {
		t.Error("engineRespawnPending = false; the value cannot reach the next load without a respawn")
	}
}

// TestApplyResidency_AdoptedEngineSaysSo: an engine waired did not spawn
// holds an environment waired cannot change and no handle waired can
// restart (waired-agent#320). Reporting that is required — a surface may
// not refuse silently (waired#1067) — and asking for a respawn we cannot
// perform would be worse than saying nothing.
func TestApplyResidency_AdoptedEngineSaysSo(t *testing.T) {
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	p.engineReconcileInFlight.Store(true)

	got := p.applyToColdEngine(infruntime.EngineModeAdopted)
	if got != management.ResidencyEffectNeedsEngineRestart {
		t.Errorf("effect = %q, want %q", got, management.ResidencyEffectNeedsEngineRestart)
	}
	if p.engineRespawnPending.Load() {
		t.Error("engineRespawnPending = true; an adopted engine has no process for us to respawn")
	}
}

// TestApplyResidency_ParkedReportsNextStart: a parked engine has no
// process to carry the value and no model to re-stamp. It is not a
// failure and it is not live, and calling it live is the claim
// waired-agent#908 removed.
func TestApplyResidency_ParkedReportsNextStart(t *testing.T) {
	ad := testOllamaAdapter(t, "http://127.0.0.1:1", 0)
	if err := ad.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	p := &agentInferenceProvider{ollama: ad}
	got, err := p.ApplyResidency(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("ApplyResidency: %v", err)
	}
	if got != management.ResidencyEffectOnEngineStart {
		t.Errorf("effect = %q, want %q", got, management.ResidencyEffectOnEngineStart)
	}
	if ad.KeepAliveDuration() != 30*time.Minute {
		t.Errorf("setting = %v, want 30m even though the engine is parked", ad.KeepAliveDuration())
	}
}

// TestApplyResidency_ResidentModelIsRestampedNotReloaded pins the other
// half: with a model in memory the change rides a per-request keep_alive
// on the SAME tag, which moves expires_at without touching the weights.
func TestApplyResidency_ResidentModelIsRestamped(t *testing.T) {
	var gotKeepAlive, gotModel atomic.Value
	gotKeepAlive.Store("")
	gotModel.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			_ = json.NewEncoder(w).Encode(psResponse{Models: []psModel{{Name: "m:q4", ExpiresAt: "2026-08-20T02:30:00Z"}}})
		case "/api/generate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ka, ok := body["keep_alive"].(string); ok {
				gotKeepAlive.Store(ka)
			}
			if m, ok := body["model"].(string); ok {
				gotModel.Store(m)
			}
			_, _ = w.Write([]byte(`{"done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, srv.URL, time.Hour)}
	p.engineReconcileInFlight.Store(true)
	got, err := p.ApplyResidency(context.Background(), 0)
	if err != nil {
		t.Fatalf("ApplyResidency: %v", err)
	}
	if got != management.ResidencyEffectLive {
		t.Errorf("effect = %q, want %q", got, management.ResidencyEffectLive)
	}
	// Asserted as a duration the engine can parse, not as a literal: the
	// literal this used to pin ("-1") is answered with a 400 by a real
	// engine (waired-agent#927), and a fake that accepts any body cannot
	// tell you that.
	ka := gotKeepAlive.Load().(string)
	d, err := time.ParseDuration(ka)
	if err != nil {
		t.Errorf("keep_alive = %q, which the engine cannot parse: %v", ka, err)
	} else if d >= 0 {
		t.Errorf("keep_alive = %q (%v), want a negative duration for indefinite", ka, d)
	}
	if m := gotModel.Load().(string); m != "m:q4" {
		t.Errorf("re-stamped %q, want the resident tag m:q4", m)
	}
	if p.engineRespawnPending.Load() {
		t.Error("engineRespawnPending = true; a resident model must be re-stamped, never bounced out of memory")
	}
}

// TestRequestEngineRespawn_SurvivesADroppedRequest is the regression for
// the window that made "(the engine restarted to pick it up)" a claim
// the product could not keep.
//
// requestEngineReconcile drops a request when a reconcile is already
// running, on the premise that the running one re-reads the intent.
// reconcileEngineServe has more than a dozen return paths, so it can
// instead consume its Swap and then exit — and a residency change may be
// the only thing happening on the host, so nothing else comes along to
// re-ask. Without the chase the flag sits set, the engine keeps the old
// spawn environment, and the operator has been told otherwise.
func TestRequestEngineRespawn_SurvivesADroppedRequest(t *testing.T) {
	oldInterval := respawnChaseInterval
	respawnChaseInterval = time.Millisecond
	t.Cleanup(func() { respawnChaseInterval = oldInterval })

	var asks atomic.Int32
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	// The first ask is dropped, standing in for a reconcile that is
	// already running and has already passed its Swap.
	p.askReconcileFn = func(bool) { asks.Add(1) }

	p.requestEngineRespawn()
	if got := asks.Load(); got != 1 {
		t.Fatalf("asks after the first request = %d, want 1", got)
	}
	if !p.engineRespawnPending.Load() {
		t.Fatal("the request was consumed; this test needs it dropped to be meaningful")
	}

	// That reconcile exits without looping — the case the coalescer's
	// premise does not cover. Nothing else on the host will ask.
	deadline := time.Now().Add(2 * time.Second)
	for asks.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("nobody re-asked; the respawn was dropped and the setting silently does not govern the next load")
		}
		time.Sleep(time.Millisecond)
	}
	if !p.engineRespawnPending.Load() {
		t.Error("the pending flag was cleared without a reconcile consuming it")
	}
	p.engineRespawnPending.Store(false) // let the chase exit
}

// TestRequestEngineRespawn_ChaseStopsWhenConsumed: the chase must not
// keep re-asking forever once a reconcile has taken the flag, or a host
// that cannot reconcile at all would spin for the life of the daemon.
func TestRequestEngineRespawn_ChaseStopsWhenConsumed(t *testing.T) {
	oldInterval, oldAttempts := respawnChaseInterval, respawnChaseAttempts
	respawnChaseInterval, respawnChaseAttempts = time.Millisecond, 5
	t.Cleanup(func() { respawnChaseInterval, respawnChaseAttempts = oldInterval, oldAttempts })

	var asks atomic.Int32
	p := &agentInferenceProvider{ollama: testOllamaAdapter(t, "http://127.0.0.1:1", 0)}
	p.askReconcileFn = func(bool) { asks.Add(1) }
	p.requestEngineRespawn()

	// A reconcile takes the flag.
	p.engineRespawnPending.Store(false)
	after := asks.Load()

	time.Sleep(50 * time.Millisecond) // longer than 5 attempts at 1ms
	if got := asks.Load(); got != after {
		t.Errorf("asks went %d -> %d; the chase kept re-asking after the flag was consumed", after, got)
	}
}
