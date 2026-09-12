package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// swapTagSize installs a recording fake for the duration of one test and
// hands back a reader for what the job actually asked about. The fake
// takes the real argument (CLAUDE.md §Test discipline) because "which tag
// was the size read for" is the assertion that makes a retry loop asking
// once per attempt, or asking about a stale tag, writable.
func swapTagSize(t *testing.T, size int64, err error) func() []string {
	t.Helper()
	var (
		mu   sync.Mutex
		tags []string
	)
	prev := tagSizeFn
	tagSizeFn = func(_ context.Context, tag string) (int64, error) {
		mu.Lock()
		tags = append(tags, tag)
		mu.Unlock()
		return size, err
	}
	t.Cleanup(func() { tagSizeFn = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), tags...)
	}
}

// PRODUCT CONTRACT (waired-agent#1299, owner ruling 2026-09-12 that the
// bar is one bar): a pull reads its whole size before it starts, so the
// download bar's total is right from the first frame instead of climbing
// each time ollama reaches another layer.
func TestRunPullJob_ReadsTheWholeSizeBeforeTheFirstByte(t *testing.T) {
	const whole = int64(17_419_957_954)
	// One layer of a three-layer tag, reported while the pull runs. The
	// bar's total must be the manifest's figure, not this layer's.
	r := &scriptedRunner{results: []error{nil}}
	tags := swapTagSize(t, whole, nil)
	p := retryProvider(t, r)

	var seen []int64
	r.observe = func() string {
		p.dlProgress.observe("model-a", download.Progress{
			Digest: "layer-1", Completed: 300_000_000, Total: 908_000_000,
		})
		_, total, _, _ := p.dlProgress.aggregate("model-a")
		seen = append(seen, total)
		return ""
	}

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := tags(); len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags the size was read for = %v, want exactly [a:q4]", got)
	}
	for _, total := range seen {
		if total != whole {
			t.Errorf("bar total during the pull = %d, want the manifest's %d", total, whole)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the runner never ran, so nothing was observed")
	}
}

// PRODUCT CONTRACT: a registry that cannot answer costs the bar its
// up-front total and nothing else. The download still runs and still
// finishes — this is the arm every release before the seeding shipped.
func TestRunPullJob_DownloadsAnywayWhenTheSizeIsUnknown(t *testing.T) {
	r := &scriptedRunner{results: []error{nil}}
	swapTagSize(t, 0, errors.New("registry unreachable"))
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateReady {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateReady)
	}
}

// PRODUCT CONTRACT: the registry is asked once per tag, not once per
// attempt. A pull that is retried three times must not make three
// requests for a figure that cannot have changed.
func TestRunPullJob_ReadsTheSizeOncePerTagAcrossRetries(t *testing.T) {
	r := &scriptedRunner{results: []error{
		errors.New("registry throttled the request"),
		errors.New("connection reset by peer"),
		nil,
	}}
	tags := swapTagSize(t, 17_419_957_954, nil)
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != 3 {
		t.Fatalf("pull attempts = %d, want 3", got)
	}
	if got := tags(); len(got) != 1 {
		t.Errorf("size reads = %v, want one for the single tag across all three attempts", got)
	}
}
