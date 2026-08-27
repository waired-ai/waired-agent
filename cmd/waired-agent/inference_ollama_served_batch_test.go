package main

import "testing"

// TestServedOllamaTuning is the whole of waired-agent#1064: the tuning
// that gets RECORDED has to describe the tag the engine will actually
// load, not the one the sizing asked for.
//
// A record of today's behaviour, not a product contract — but the
// behaviour it records was measured. On sv-mag, 2026-08-27, /api/create
// timed out during a first load and the agent logged "serving base tag
// with automatic batch" while /inference/status went on reporting
// num_batch 2048 with no -wb2048 in the resident tag.
func TestServedOllamaTuning(t *testing.T) {
	forced := func(n int) ollamaTuning {
		var tune ollamaTuning
		tune.ContextLength = 200704
		tune.NumBatch = n
		return tune
	}
	cases := []struct {
		name         string
		in           ollamaTuning
		derivedInUse bool
		want         int
	}{
		{
			name:         "derived model in use keeps the forced batch",
			in:           forced(ollamaLargeBatch),
			derivedInUse: true,
			want:         ollamaLargeBatch,
		},
		{
			name: "derived model unavailable reports the engine's own sizing",
			in:   forced(ollamaLargeBatch),
			want: 0,
		},
		{
			// The override never fired, so there is nothing to correct
			// and nothing to claim.
			name: "no forced batch is left alone",
			in:   forced(0),
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := servedOllamaTuning(c.in, c.derivedInUse)
			if got.NumBatch != c.want {
				t.Errorf("NumBatch = %d, want %d", got.NumBatch, c.want)
			}
			if got.ContextLength != c.in.ContextLength {
				t.Errorf("ContextLength = %d, want %d untouched", got.ContextLength, c.in.ContextLength)
			}
		})
	}
}

// TestServedOllamaTuning_LeavesTheTagRungNothingToDrop is the reason
// waired-agent#1064 is worth fixing rather than just reporting: #1054's
// post-load ladder keys its first rung on the forced batch, so an
// uncorrected intent makes the ladder spend an allocation probe
// dropping a batch that was never applied — and persist a refusal for
// it (dropForcedOllamaBatch stamps ForcedBatchRefusedAt), which
// suppresses the override on every later sizing of this variant.
func TestServedOllamaTuning_LeavesTheTagRungNothingToDrop(t *testing.T) {
	m, v, hw, asked := anchorSpillFixture()
	if asked.NumBatch < ollamaLargeBatch {
		t.Fatalf("fixture should force the batch: NumBatch = %d", asked.NumBatch)
	}

	// Asked for and applied: the ladder's first rung is the tag step.
	if _, _, kind := degradeStep(asked, m, v, hw, tuningVRAMExhausted, "probe"); kind != stepTag {
		t.Fatalf("with the forced batch applied, the first rung = %v, want stepTag", kind)
	}

	// Asked for and NOT applied: the batch rung must not be offered.
	served := servedOllamaTuning(asked, false)
	if _, _, kind := degradeStep(served, m, v, hw, tuningVRAMExhausted, "probe"); kind == stepTag {
		t.Error("the ladder offered a tag step for a batch the engine never got")
	}
}
