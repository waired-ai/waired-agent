package main

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// warmConversationSlots is how many conversations this host can hold
// WARM at once — the quantity Capacity means since waired-agent#1126.
//
// It replaced a decode-throughput quotient, floor(decode_tokps / 30),
// which answered a different question from the one routing asks. That
// figure was never clamped by the engine's KV slot count, so a host
// benching at 120 tok/s advertised 4 into an engine started with
// OLLAMA_NUM_PARALLEL of 1 — and on that engine a slot IS the unit of
// retention, so requests 3 and 4 evicted the prefixes of 1 and 2.
// Prefix loss costs a full re-prefill: 2.57 s for an appended turn
// against 35.38 s for one whose prefix was lost, on a 33.85 s cold
// value (docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md).
//
// Owner ruling, 2026-08-29, waired-agent#1126:
//
//	Capacity と保持できる会話数を一致させるのが、コーディングエージェント
//	用途を中心とする waired には適切でしょう
//
// and, on what to divide by:
//
//	窓にしましょう
//
// The two engines express the same quantity differently, so this reads
// each one where it actually keeps it rather than modelling one on the
// other:
//
//   - ollama / llama.cpp partitions KV into slots, each holding one
//     window. The slot count IS the answer. Prefer what the runner is
//     really serving (ObservedNumParallel, read off its command line
//     after load — #763/#846) over what was asked for, because the
//     engine silently caps the request when the per-slot KV does not
//     fit; fall back to the intent, then to the sizing's own ceiling.
//   - vLLM keeps one shared pool of paged blocks, hashed by content, so
//     the answer is the pool divided by the served window. Dividing by
//     the WINDOW rather than by a typical conversation length is what
//     makes the two engines mean the same thing, and it errs
//     conservative: real conversations are shorter, so a host usually
//     holds more than it claims.
//
// Returns 0 for "not known yet", which every caller reads as the
// unmeasured fail-safe rather than as a ceiling of zero (on the wire 0
// means UNLIMITED — see proto/signer's InferenceState.Capacity).
func warmConversationSlots(engineKind string, t infruntime.ModelTuning) int {
	switch engineKind {
	case signer.InferenceTypeOllama:
		if t.ObservedNumParallel > 0 {
			return t.ObservedNumParallel
		}
		if t.NumParallel > 0 {
			return t.NumParallel
		}
		if t.RecommendedMaxParallel > 0 {
			return t.RecommendedMaxParallel
		}
		return 0
	case signer.InferenceTypeVLLM:
		if t.KVCapacityTokens <= 0 || t.ContextLength <= 0 {
			return 0
		}
		if n := t.KVCapacityTokens / t.ContextLength; n > 0 {
			return n
		}
		// The engine refuses to start with a pool below the window, so
		// this is unreachable in practice; one is the honest answer if
		// it ever happens.
		return 1
	default:
		return 0
	}
}

// WarmConversationSlots is warmConversationSlots against the tuning the
// SERVING engine actually applied. 0 when there is no engine, no tuning
// yet, or a tuning this build cannot read a slot count out of.
//
// Live rather than latched: the tuning is applied when the engine
// spawns, which can be after the boot benchmark has already run, and it
// changes again whenever the operator switches model. Reading it per
// call is what lets the advertised figure follow without a re-benchmark.
func (p *agentInferenceProvider) WarmConversationSlots() int {
	if p == nil {
		return 0
	}
	if p.servingEngine() == catalog.RuntimeVLLM {
		// The adapter is the linux-only VLLMAdapter behind the Adapter
		// interface, so reach the tuning through an assertion this
		// untagged file compiles with on every platform — the same shape
		// every other vLLM tuning read site uses.
		tuner, ok := p.vllmAdapter().(interface {
			AppliedTuning() infruntime.ModelTuning
		})
		if !ok {
			return 0
		}
		return warmConversationSlots(signer.InferenceTypeVLLM, tuner.AppliedTuning())
	}
	if p.ollama == nil {
		return 0
	}
	return warmConversationSlots(signer.InferenceTypeOllama, p.ollama.AppliedTuning())
}
