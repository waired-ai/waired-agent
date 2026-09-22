//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// partialWritingRunner stands in for `hf download` the way the real one
// behaves when it is killed: it parks a partial under the --local-dir it
// was given, then fails. It keeps each invocation's argv, and reports
// whether partials from earlier were still there when it started.
type partialWritingRunner struct {
	mu       sync.Mutex
	args     [][]string
	stale    []int // partials found at the start of each invocation
	err      error
	lines    []string      // output before failing; "not found" stops the puller's retry
	block    chan struct{} // when non-nil, Run waits on it (or ctx) before failing
	inFlight atomic.Int32
	maxAtOne atomic.Int32
}

func localDirArg(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--local-dir" {
			return args[i+1]
		}
	}
	return ""
}

func countPartials(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(dir, ".cache/huggingface/download"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(d.Name()) == ".incomplete" {
			n++
		}
		return nil
	})
	return n
}

func (r *partialWritingRunner) Run(ctx context.Context, _ string, args, _ []string, onLine func(string)) error {
	now := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		m := r.maxAtOne.Load()
		if now <= m || r.maxAtOne.CompareAndSwap(m, now) {
			break
		}
	}
	dir := localDirArg(args)
	stale := 0
	_ = filepath.WalkDir(filepath.Join(dir, ".cache/huggingface/download"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(d.Name()) == ".incomplete" {
			stale++
		}
		return nil
	})
	r.mu.Lock()
	r.args = append(r.args, append([]string(nil), args...))
	r.stale = append(r.stale, stale)
	n := len(r.args)
	r.mu.Unlock()
	cache := filepath.Join(dir, ".cache/huggingface/download")
	_ = os.MkdirAll(cache, 0o755)
	_ = os.WriteFile(filepath.Join(cache, "shard.etag."+string(rune('a'+n))+"0000000.incomplete"), make([]byte, 1024), 0o644)
	for _, l := range r.lines {
		onLine(l)
	}
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return errors.New("signal: killed")
		}
	}
	return r.err
}

func hfPartialsProvider(t *testing.T) (*agentInferenceProvider, catalog.Manifest, catalog.Variant) {
	t.Helper()
	p := vllmTestProvider(t)
	// No network: an unreadable listing falls back to the whole repository.
	p.hfFiles = fakeHFLister{err: io.ErrUnexpectedEOF}
	m := mixedVLLMManifest()
	return p, m, m.Variants[1]
}

// PRODUCT CONTRACT (waired-agent#1519): a download that stops leaves no
// partial file behind, and the next one starts without the previous one's.
// huggingface_hub never resumes them, and on the host that found this three
// interrupted attempts had left 17.8 GB.
func TestDownloadHFWeights_LeavesNoPartialFiles(t *testing.T) {
	p, m, v := hfPartialsProvider(t)
	dir := p.hfLocalDir(m.ModelID, v)
	// What a killed attempt of an earlier run left.
	writeHFFile(t, dir, ".cache/huggingface/download/shard.etag.deadbeef.incomplete", 4096)

	r := &partialWritingRunner{err: errors.New("exit status 1")}
	puller := download.NewHFPuller("hf-fake", r)
	if _, err := p.downloadHFWeights(context.Background(), m.ModelID, v, puller, false, nil); err == nil {
		t.Fatal("expected the download to fail")
	}

	if len(r.stale) == 0 || r.stale[0] != 0 {
		t.Errorf("the download started with %v stale partials in its directory, want 0", r.stale)
	}
	if n := countPartials(t, dir); n != 0 {
		t.Errorf("%d partial files remain after the download stopped, want 0", n)
	}
	st, _ := p.store.Load()
	if got := st.VLLMModels[m.ModelID].State; got != catalog.ModelStateFailed {
		t.Errorf("state = %q after a failed download, want failed", got)
	}
}

// A stop somebody asked for is not a failure: the ollama path records
// nothing on a cancel, and the HF path now does the same, leaving the row
// to settleCancelledPull. Before waired-agent#1519 it wrote "failed" and a
// WARN for every `waired models cancel`.
func TestDownloadHFWeights_ARequestedStopRecordsNoFailure(t *testing.T) {
	p, m, v := hfPartialsProvider(t)
	dir := p.hfLocalDir(m.ModelID, v)
	if err := p.store.Update(func(s *catalog.State) {
		s.VLLMModels[m.ModelID] = catalog.ModelState{VariantID: v.VariantID, State: catalog.ModelStateQueued}
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &partialWritingRunner{block: make(chan struct{})}
	puller := download.NewHFPuller("hf-fake", r)
	var stopped atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := p.downloadHFWeights(ctx, m.ModelID, v, puller, false, stopped.Load)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stopped.Store(true)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a stopped download reported success")
	}

	if len(r.args) != 1 {
		t.Errorf("hf ran %d times, want once: a stopped download must not be retried", len(r.args))
	}
	st, _ := p.store.Load()
	if ms := st.VLLMModels[m.ModelID]; ms.State == catalog.ModelStateFailed || ms.Error != "" {
		t.Errorf("a requested stop was recorded as a failure: state=%q error=%q", ms.State, ms.Error)
	}
	if n := countPartials(t, dir); n != 0 {
		t.Errorf("%d partial files remain after a cancel, want 0", n)
	}
}

// Two downloads into one directory — the probe and a pull of the same
// repository — run one after the other, so neither sweeps a partial the
// other is still writing.
func TestDownloadHFWeights_OneDownloadPerDirectory(t *testing.T) {
	p, m, v := hfPartialsProvider(t)
	// "not found" ends each download after one invocation, so every
	// invocation below is the start of a download.
	r := &partialWritingRunner{block: make(chan struct{}), err: errors.New("exit status 1"),
		lines: []string{"Repository Not Found"}}
	puller := download.NewHFPuller("hf-fake", r)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.downloadHFWeights(context.Background(), m.ModelID, v, puller, false, nil)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(r.block)
	wg.Wait()

	if len(r.stale) != 2 {
		t.Fatalf("hf ran %d times, want 2", len(r.stale))
	}
	if got := r.maxAtOne.Load(); got != 1 {
		t.Errorf("%d downloads wrote to the same directory at once, want 1", got)
	}
	for i, s := range r.stale {
		if s != 0 {
			t.Errorf("invocation %d started beside %d partials of another; want 0", i, s)
		}
	}
}

// A custom model's weights directory names its commit, so two imports of one
// repository at different commits never share one; a bundled build keeps the
// directory it always had (waired-ai/waired#1480).
func TestHFLocalDir_ACustomModelNamesItsCommit(t *testing.T) {
	p, m, v := hfPartialsProvider(t)
	if got := filepath.Base(p.hfLocalDir(m.ModelID, v)); got != "openai__gpt-oss-20b" {
		t.Errorf("bundled dir %q", got)
	}
	cv := v
	cv.Source.Revision = "0123456789abcdef0123456789abcdef01234567"
	a := p.hfLocalDir("custom-gpt-oss-0123abcd", cv)
	cv.Source.Revision = "fedcba9876543210fedcba9876543210fedcba98"
	b := p.hfLocalDir("custom-gpt-oss-89abcdef", cv)
	if filepath.Base(a) != "openai__gpt-oss-20b@0123456789ab" || a == b || !hfDirOwnedBy(p.stateDir, a) {
		t.Errorf("custom dirs %q and %q", a, b)
	}
}

// A custom model is pulled narrowly or not at all: with no safetensors at the
// top level there is nothing to fetch, and the whole-repository fallback a
// bundled build takes would fetch whatever the repository holds
// (waired-ai/waired#1480).
func TestDownloadHFWeights_ACustomModelWithoutSafetensorsIsRefused(t *testing.T) {
	p, _, v := hfPartialsProvider(t)
	p.hfFiles = fakeHFLister{files: []download.HFRepoFile{{Name: "config.json", Size: 1}, {Name: "pytorch_model.bin", Size: 9}}}
	runner := &fakeHFRunner{lines: []string{"done"}}
	v.Source.Revision = "0123456789abcdef0123456789abcdef01234567"
	_, err := p.downloadHFWeights(context.Background(), "custom-x-0123abcd", v, download.NewHFPuller("hf-fake", runner), false, nil)
	if err == nil || !strings.Contains(err.Error(), "no safetensors") {
		t.Fatalf("err %v, want the no-safetensors refusal", err)
	}
	if n := len(runner.args); n != 0 {
		t.Errorf("hf download ran %d times", n)
	}
	st, _ := p.store.Load()
	if ms := st.VLLMModels["custom-x-0123abcd"]; ms.State != catalog.ModelStateFailed || ms.Error == "" {
		t.Errorf("record %+v, want failed with the reason", ms)
	}
}

// The disk is checked before a byte is fetched, as the ollama pull does
// (waired-ai/waired#1480).
func TestDownloadHFWeights_StopsBeforeAFullDisk(t *testing.T) {
	p, m, v := hfPartialsProvider(t)
	p.hfFiles = fakeHFLister{files: []download.HFRepoFile{{Name: "config.json", Size: 1}, {Name: "model.safetensors", Size: 20 << 30}}}
	prev := freeDiskFn
	t.Cleanup(func() { freeDiskFn = prev })
	freeDiskFn = func(string) (int64, error) { return 5 << 30, nil }
	runner := &fakeHFRunner{lines: []string{"done"}}
	_, err := p.downloadHFWeights(context.Background(), m.ModelID, v, download.NewHFPuller("hf-fake", runner), false, nil)
	if n := len(runner.args); !errors.Is(err, errDiskShort) || n != 0 {
		t.Fatalf("err %v after %d runs, want errDiskShort before any", err, n)
	}
}
