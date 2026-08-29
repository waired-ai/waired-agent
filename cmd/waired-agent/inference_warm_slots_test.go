package main

import (
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestWarmConversationSlots is the table behind what Capacity means
// since waired-agent#1126: how many conversations this host holds warm.
//
// Product contract — owner ruling, 2026-08-29, waired-agent#1126
// ("Capacity と保持できる会話数を一致させる" / "窓にしましょう").
func TestWarmConversationSlots(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		tuning infruntime.ModelTuning
		want   int
	}{
		// --- ollama: a slot IS the unit of retention ---
		{
			name:   "ollama prefers what the runner is really serving",
			engine: signer.InferenceTypeOllama,
			// The engine silently caps OLLAMA_NUM_PARALLEL when the
			// per-slot KV does not fit the window, so the intent (2) is
			// not what retains anything — the runner's own -np is (#763).
			tuning: infruntime.ModelTuning{NumParallel: 2, ObservedNumParallel: 1, RecommendedMaxParallel: 4},
			want:   1,
		},
		{
			name:   "ollama falls back to the exported intent",
			engine: signer.InferenceTypeOllama,
			tuning: infruntime.ModelTuning{NumParallel: 2, RecommendedMaxParallel: 4},
			want:   2,
		},
		{
			name:   "ollama falls back to the sizing ceiling",
			engine: signer.InferenceTypeOllama,
			tuning: infruntime.ModelTuning{RecommendedMaxParallel: 4},
			want:   4,
		},
		{
			name:   "ollama with no tuning is not known yet",
			engine: signer.InferenceTypeOllama,
			tuning: infruntime.ModelTuning{},
			want:   0,
		},
		{
			name:   "ollama ignores a vLLM-shaped pool",
			engine: signer.InferenceTypeOllama,
			tuning: infruntime.ModelTuning{KVCapacityTokens: 339160, ContextLength: 124928},
			want:   0,
		},

		// --- vLLM: one shared pool, divided by the served window ---
		{
			name:   "vllm divides the pool by the window",
			engine: signer.InferenceTypeVLLM,
			// Measured on the pinned engine: 339,160 tokens of pool at a
			// 124,928-token window.
			tuning: infruntime.ModelTuning{KVCapacityTokens: 339160, ContextLength: 124928},
			want:   2,
		},
		{
			name:   "vllm reports the pool it actually got, not one carried over",
			engine: signer.InferenceTypeVLLM,
			// The pool is a function of the free VRAM at profiling time,
			// so the same host reports different figures across starts:
			// 393,709 on vLLM 0.24.0 and 339,160 on 0.28.0 with a clean
			// GPU, and 285,883 from a start that overlapped the previous
			// engine's teardown (waired-agent#1151). Read back per start.
			tuning: infruntime.ModelTuning{KVCapacityTokens: 393709, ContextLength: 124928},
			want:   3,
		},
		{
			name:   "a start that got a smaller pool holds fewer conversations",
			engine: signer.InferenceTypeVLLM,
			tuning: infruntime.ModelTuning{KVCapacityTokens: 285883, ContextLength: 124928},
			want:   2,
		},
		{
			name:   "vllm without a pool reading is not known yet",
			engine: signer.InferenceTypeVLLM,
			tuning: infruntime.ModelTuning{ContextLength: 124928},
			want:   0,
		},
		{
			name:   "vllm without a window is not known yet",
			engine: signer.InferenceTypeVLLM,
			tuning: infruntime.ModelTuning{KVCapacityTokens: 339160},
			want:   0,
		},
		{
			name:   "vllm ignores an ollama-shaped slot count",
			engine: signer.InferenceTypeVLLM,
			tuning: infruntime.ModelTuning{NumParallel: 2, ObservedNumParallel: 2},
			want:   0,
		},
		{
			// The engine refuses to start with a pool below the window,
			// so this cannot happen; one is the honest answer if it does.
			name:   "vllm never reports zero from a real reading",
			engine: signer.InferenceTypeVLLM,
			tuning: infruntime.ModelTuning{KVCapacityTokens: 1000, ContextLength: 124928},
			want:   1,
		},

		// --- neither ---
		{
			name:   "no engine",
			engine: signer.InferenceTypeNone,
			tuning: infruntime.ModelTuning{NumParallel: 4},
			want:   0,
		},
		{
			name:   "an engine kind this build does not drive",
			engine: "some-future-engine",
			tuning: infruntime.ModelTuning{NumParallel: 4, KVCapacityTokens: 1000, ContextLength: 100},
			want:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := warmConversationSlots(c.engine, c.tuning); got != c.want {
				t.Errorf("warmConversationSlots(%q) = %d, want %d", c.engine, got, c.want)
			}
		})
	}
}

// TestWarmConversationSlots_NeverOverAdmitsTheOldWay is the defect
// waired-agent#1126 was filed for, as a guard: a fast host on a
// one-slot engine used to advertise four.
func TestWarmConversationSlots_NeverOverAdmitsTheOldWay(t *testing.T) {
	// The reported host: ~120 tok/s of decode, engine started with
	// OLLAMA_NUM_PARALLEL=1. floor(120/30) said 4.
	got := warmConversationSlots(signer.InferenceTypeOllama, infruntime.ModelTuning{
		NumParallel:         1,
		ObservedNumParallel: 1,
		ContextLength:       131072,
	})
	if got != 1 {
		t.Errorf("warm slots = %d, want 1: the engine has one slot, and on ollama a slot IS the unit of KV retention", got)
	}
}

// TestProviderWarmConversationSlots_NilAndUnwiredAreNotKnownYet keeps
// the "0 means not known" contract at the provider boundary. On the
// wire 0 means UNLIMITED, so every caller has to be able to tell the
// two apart — capacityFn is what turns this 0 into the one-at-a-time
// fail-safe.
func TestProviderWarmConversationSlots_NilAndUnwiredAreNotKnownYet(t *testing.T) {
	var nilProv *agentInferenceProvider
	if got := nilProv.WarmConversationSlots(); got != 0 {
		t.Errorf("nil provider = %d, want 0", got)
	}
	// A provider with no ollama adapter and no vLLM adapter: the shape a
	// --disable-inference daemon and an unenrolled one both have.
	prov := &agentInferenceProvider{}
	if got := prov.WarmConversationSlots(); got != 0 {
		t.Errorf("adapterless provider = %d, want 0", got)
	}
}
