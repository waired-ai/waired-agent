package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/notice"
)

// A computer serving a custom model under 200,704 tokens says so, standing
// and Info, like the long window (waired-ai/waired#1481 item 7, #1473 ruling
// 10: no minimum, and the limit is stated). A bundled model at the coding
// window, and a custom one that reaches it, say nothing. Record of today's
// behaviour.
func TestEngineNotices_ShortContextWindow(t *testing.T) {
	find := func(p engineProvenance) *notice.Notice {
		for _, n := range engineNotices(p, true, true, true) {
			if n.Kind == notice.KindShortContextWindow {
				return &n
			}
		}
		return nil
	}
	n := find(engineProvenance{Engine: "ollama", ServedWindow: 40960, ServedModelID: "custom-tiny-0123abcd"})
	if n == nil {
		t.Fatal("no notice for a custom model served at 40,960 tokens")
	}
	if n.Severity != notice.SeverityInfo || !strings.Contains(n.Title, "40,960") || !strings.Contains(n.Title, "200,704") {
		t.Errorf("notice %+v", n)
	}
	for name, p := range map[string]engineProvenance{
		"bundled at 200,704":      {Engine: "ollama", ServedWindow: 200704, ServedModelID: "qwen3.5-9b"},
		"custom at 262,144":       {Engine: "vllm", ServedWindow: 262144, ServedModelID: "custom-big-0123abcd"},
		"nothing tuned":           {Engine: "ollama"},
		"bundled, CI's small one": {Engine: "ollama", ServedWindow: 4096, ServedModelID: "ci-tiny"},
	} {
		if n := find(p); n != nil {
			t.Errorf("%s: unexpected notice %+v", name, n)
		}
	}
}
