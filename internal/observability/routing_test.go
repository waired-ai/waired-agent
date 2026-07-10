package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRecorder_RecordPinnedPeerUnreachable_RingAndCounter(t *testing.T) {
	ring := NewRing(DefaultRingCapacity)
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	r := NewRecorder(ring, metrics, nil)

	r.RecordPinnedPeerUnreachable("dev_xyz", "qwen3-8b-instruct", "unreachable")
	r.RecordPinnedPeerUnreachable("dev_xyz", "qwen3-8b-instruct", "lacks_model")

	// Ring should have both events.
	events, _, _ := ring.Since(0, []Kind{KindPinnedPeerUnreachable}, 100)
	if len(events) != 2 {
		t.Fatalf("want 2 ring events, got %d", len(events))
	}
	if events[0].PinnedPeerUnreachable.Reason != "unreachable" {
		t.Errorf("first event reason = %q", events[0].PinnedPeerUnreachable.Reason)
	}
	if events[1].PinnedPeerUnreachable.Reason != "lacks_model" {
		t.Errorf("second event reason = %q", events[1].PinnedPeerUnreachable.Reason)
	}

	// Counters should also have ticked.
	v := testCounterValue(t, reg, "waired_inference_pinned_peer_unreachable_total", "unreachable")
	if v != 1 {
		t.Errorf("unreachable counter = %v, want 1", v)
	}
	v = testCounterValue(t, reg, "waired_inference_pinned_peer_unreachable_total", "lacks_model")
	if v != 1 {
		t.Errorf("lacks_model counter = %v, want 1", v)
	}
}

func TestRecorder_RecordPinnedPeerUnreachable_NilSafe(t *testing.T) {
	var r *Recorder
	r.RecordPinnedPeerUnreachable("dev", "m", "unreachable") // must not panic
}

func TestRecorder_RecordPinnedPeerUnreachable_PartialSinks(t *testing.T) {
	// Ring only — counters absent.
	ring := NewRing(DefaultRingCapacity)
	r := NewRecorder(ring, nil, nil)
	r.RecordPinnedPeerUnreachable("dev", "m", "unreachable")
	events, _, _ := ring.Since(0, []Kind{KindPinnedPeerUnreachable}, 10)
	if len(events) != 1 {
		t.Errorf("ring-only sink should still record event, got %d", len(events))
	}
}

func testCounterValue(t *testing.T, reg *prometheus.Registry, name, label string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
