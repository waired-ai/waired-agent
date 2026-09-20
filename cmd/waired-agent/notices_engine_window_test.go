package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/notice"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// A computer serving the long window says so, standing, wherever notices are
// shown. It is Info and not Warn: the person chose it after being told the
// cost, and `waired doctor` reports defects (waired-ai/waired#1456).
func TestEngineNotices_LongContextWindow(t *testing.T) {
	has := func(ns []notice.Notice, k notice.Kind) *notice.Notice {
		for i := range ns {
			if ns[i].Kind == k {
				return &ns[i]
			}
		}
		return nil
	}

	t.Run("serving the long window", func(t *testing.T) {
		got := engineNotices(engineProvenance{Engine: "ollama", ServedWindow: hostfit.ServingWindow1M}, true, true, true)
		n := has(got, notice.KindLongContextWindow)
		if n == nil {
			t.Fatalf("no long-window notice in %+v", got)
		}
		if n.Severity != notice.SeverityInfo {
			t.Errorf("severity = %v, want Info — a choice honoured is not a defect", n.Severity)
		}
	})

	t.Run("serving the coding window", func(t *testing.T) {
		got := engineNotices(engineProvenance{Engine: "ollama", ServedWindow: hostfit.ServingWindow200k}, true, true, true)
		if n := has(got, notice.KindLongContextWindow); n != nil {
			t.Errorf("a host on the coding window was told about the long one: %+v", n)
		}
	})

	t.Run("nothing tuned yet", func(t *testing.T) {
		got := engineNotices(engineProvenance{Engine: "ollama"}, true, true, true)
		if n := has(got, notice.KindLongContextWindow); n != nil {
			t.Errorf("a host with no tuning was told about the long window: %+v", n)
		}
	})

	// Two facts about the engine are two notices — the lesson of #1229,
	// which this must not undo.
	t.Run("beside a version warning, not instead of it", func(t *testing.T) {
		got := engineNotices(engineProvenance{
			Engine: "ollama", ServedWindow: hostfit.ServingWindow1M, VersionWarning: "older than the pin",
		}, true, true, true)
		if has(got, notice.KindLongContextWindow) == nil || has(got, notice.KindEngineVersion) == nil {
			t.Errorf("want both notices, got %+v", got)
		}
	})
}
