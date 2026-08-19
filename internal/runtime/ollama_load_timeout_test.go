package runtime

import (
	"testing"
	"time"
)

// TestOllamaAdapter_ProcessEnv_SetsTheLoadDeadline pins the delivery of
// OllamaLoadTimeout to `ollama serve`.
//
// The engine's own default is 5 minutes, and a load it kills leaves nothing
// resident — so the next request repeats the whole cost and can be killed the
// same way. On the Ryzen AI Max+ 395 host that turned one slow 75.8 GB load
// into an unbounded retry loop (waired-ai/waired-agent#837). Unconditional,
// like the residency cap beside it: it is an engine-wide deadline, not part
// of the #621 per-model tuning, so it must be there before any tuning exists.
func TestOllamaAdapter_ProcessEnv_SetsTheLoadDeadline(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	env := a.processEnv()
	if !contains(env, "OLLAMA_LOAD_TIMEOUT="+OllamaLoadTimeout) {
		t.Errorf("untuned serve env missing the load deadline: %v", ollamaOnly(env))
	}
	if n := countPrefix(env, "OLLAMA_LOAD_TIMEOUT="); n != 1 {
		t.Errorf("OLLAMA_LOAD_TIMEOUT= appears %d times, want exactly 1", n)
	}

	// A computed tuning must not displace it: the deadline is not one of the
	// tuning keys, which are dropped as a set whenever a tuning exists.
	b := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	b.SetModelEnv([]string{
		"OLLAMA_CONTEXT_LENGTH=200704",
		"OLLAMA_KV_CACHE_TYPE=q8_0",
		"OLLAMA_NUM_PARALLEL=1",
	})
	if benv := b.processEnv(); !contains(benv, "OLLAMA_LOAD_TIMEOUT="+OllamaLoadTimeout) {
		t.Errorf("tuned serve env missing the load deadline: %v", ollamaOnly(benv))
	}
}

// TestOllamaAdapter_ProcessEnv_LoadDeadlineYieldsToTheOperator is the override
// half, on the same terms as the residency cap: an /etc/waired/agent.env line
// reaches the engine, and set-but-empty asks for the engine's own default.
func TestOllamaAdapter_ProcessEnv_LoadDeadlineYieldsToTheOperator(t *testing.T) {
	t.Setenv("OLLAMA_LOAD_TIMEOUT", "30m")

	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	env := a.processEnv()
	if !contains(env, "OLLAMA_LOAD_TIMEOUT=30m") {
		t.Errorf("the operator's load deadline must reach the engine: %v", ollamaOnly(env))
	}
	if contains(env, "OLLAMA_LOAD_TIMEOUT="+OllamaLoadTimeout) {
		t.Errorf("our default must not be emitted alongside the operator's: %v", ollamaOnly(env))
	}
	if n := countPrefix(env, "OLLAMA_LOAD_TIMEOUT="); n != 1 {
		t.Errorf("OLLAMA_LOAD_TIMEOUT= appears %d times, want exactly 1", n)
	}

	t.Setenv("OLLAMA_LOAD_TIMEOUT", "")
	b := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	if benv := b.processEnv(); contains(benv, "OLLAMA_LOAD_TIMEOUT="+OllamaLoadTimeout) {
		t.Errorf("an explicitly cleared deadline must not be re-armed: %v", ollamaOnly(benv))
	}
}

// TestOllamaLoadTimeout_ExceedsTheEngineDefault records why the constant
// exists at all: it is only worth exporting if it is longer than the 5 minutes
// the engine would use on its own. A future edit that shortens it below that
// is silently a no-op, so pin the direction rather than the digits.
func TestOllamaLoadTimeout_ExceedsTheEngineDefault(t *testing.T) {
	const engineDefault = 5 * time.Minute
	d, err := time.ParseDuration(OllamaLoadTimeout)
	if err != nil {
		t.Fatalf("OllamaLoadTimeout = %q is not a duration the engine can parse: %v", OllamaLoadTimeout, err)
	}
	if d <= engineDefault {
		t.Errorf("OllamaLoadTimeout = %v, which does not exceed the engine's own %v default — exporting it changes nothing", d, engineDefault)
	}
}

// TestServeInputsEqual_NoMmapIsAServeInput pins NoMmap into the bounce
// predicate. It selects a different derived model, so two tunings that differ
// only there are two different engine processes; leaving it out would latch a
// host on the tag it happened to boot with.
func TestServeInputsEqual_NoMmapIsAServeInput(t *testing.T) {
	base := ModelTuning{
		ModelID: "m", VariantID: "v", ContextLength: 200704,
		NumParallel: 1, KVCacheType: "q8_0", FlashAttention: true,
	}
	mapped, unmapped := base, base
	unmapped.NoMmap = true

	if mapped.ServeInputsEqual(unmapped) {
		t.Error("tunings differing only in NoMmap compared equal; the derived tag differs, so the engine process does too")
	}
	if !mapped.ServeInputsEqual(mapped) || !unmapped.ServeInputsEqual(unmapped) {
		t.Error("a tuning must compare equal to itself")
	}
	// The outcome fields this predicate deliberately ignores still must not
	// move it — NoMmap being an input is the claim, not "any new field".
	noisy := unmapped
	noisy.Verified = true
	noisy.Warning = "something happened"
	if !unmapped.ServeInputsEqual(noisy) {
		t.Error("post-spawn outcome fields must not make a tuning look like a different engine")
	}
}
