package disco

import (
	"math"
	"testing"
	"time"
)

// TestRTTSnapshot_EmptyWhenNoSamples confirms RTTSnapshot returns nil
// before any pongs have arrived — keeps the Phase 7 InferenceState
// push compact for fresh agents.
func TestRTTSnapshot_EmptyWhenNoSamples(t *testing.T) {
	s := &Service{rttEMA: map[string]float64{}}
	if got := s.RTTSnapshot(); got != nil {
		t.Errorf("RTTSnapshot() on fresh Service = %+v, want nil", got)
	}
}

// TestRTTSnapshot_RoundsToNearestMS verifies the snapshot rounds the
// internal float64 EMA to the nearest whole millisecond. The wire
// format uses uint32 to keep NetworkMap entries small, and uint32
// truncation (vs rounding) would systematically bias every peer's
// RTT downward by ~0.5 ms.
func TestRTTSnapshot_RoundsToNearestMS(t *testing.T) {
	s := &Service{rttEMA: map[string]float64{
		"peer-a": 12.4,
		"peer-b": 12.6,
		"peer-c": 12.5,
		"peer-d": 0.0,
	}}
	got := s.RTTSnapshot()
	want := map[string]uint32{
		"peer-a": 12,
		"peer-b": 13,
		"peer-c": 13,
		"peer-d": 0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RTTSnapshot()[%q] = %d, want %d", k, got[k], v)
		}
	}
}

// TestRecordRTTSample_FirstSampleSeedsEMA confirms the first sample
// for a peer becomes the EMA value directly (no warmup blending with
// a zero baseline, which would otherwise halve every first reading).
func TestRecordRTTSample_FirstSampleSeedsEMA(t *testing.T) {
	s := &Service{rttEMA: map[string]float64{}}
	s.recordRTTSample("peer-a", 20*time.Millisecond)
	if got := s.rttEMA["peer-a"]; math.Abs(got-20.0) > 1e-9 {
		t.Errorf("first sample EMA = %f, want 20.0 (no half-of-zero warmup)", got)
	}
}

// TestRecordRTTSample_EMAConvergesTowardsNewValue exercises the
// α = 0.3 smoothing. After 5 consecutive samples at 100 ms following
// an initial 10 ms reading, the EMA should be visibly closer to 100
// than to 10 but not yet at 100 — that's the "fast enough to catch
// path changes, slow enough to ignore single spikes" contract.
func TestRecordRTTSample_EMAConvergesTowardsNewValue(t *testing.T) {
	s := &Service{rttEMA: map[string]float64{}}
	s.recordRTTSample("peer-a", 10*time.Millisecond)
	for i := 0; i < 5; i++ {
		s.recordRTTSample("peer-a", 100*time.Millisecond)
	}
	got := s.rttEMA["peer-a"]
	if got <= 50.0 {
		t.Errorf("after 5 samples at 100 ms following 10 ms baseline, EMA = %f, want > 50 (smoothing too slow)", got)
	}
	if got >= 100.0 {
		t.Errorf("EMA = %f, want < 100 (smoothing too fast — single outlier would saturate)", got)
	}
}

// TestRecordRTTSample_IgnoresInvalidInputs confirms the guard rails:
// empty deviceID or non-positive rtt are no-ops. A zero or negative
// duration could come from clock skew between sentAt and now;
// distorting the EMA with that data would mislead the Selector.
func TestRecordRTTSample_IgnoresInvalidInputs(t *testing.T) {
	s := &Service{rttEMA: map[string]float64{"peer-a": 50.0}}
	s.recordRTTSample("", 10*time.Millisecond)
	s.recordRTTSample("peer-a", 0)
	s.recordRTTSample("peer-a", -5*time.Millisecond)
	if got := s.rttEMA["peer-a"]; math.Abs(got-50.0) > 1e-9 {
		t.Errorf("recordRTTSample with invalid inputs mutated EMA: peer-a = %f, want 50.0", got)
	}
	if _, ok := s.rttEMA[""]; ok {
		t.Error("empty deviceID created an entry; should be no-op")
	}
}
