//go:build !linux

package runtime

// applyEngineOOMScore is Linux-only. Darwin has no public way to bias
// jetsam's choice — it already prefers the largest consumer, which is the
// engine — and Windows bounds the engine with a Job Object instead of
// choosing a victim after the fact. Both are stated in oom_score_linux.go.
func applyEngineOOMScore(int) error { return nil }
