package download

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HFHubBaseURL is the public Hugging Face Hub origin.
const HFHubBaseURL = "https://huggingface.co"

// Untagged, unlike hf.go beside it. That file carries
// `linux || darwin` because it spawns the CLI and reaches for
// syscall.SysProcAttr; nothing here does, and cmd/waired-agent holds an
// HFFileLister in a struct field that every OS compiles
// (waired-agent#1298). A tag copied from a neighbour is how that field
// became `undefined: download.HFFileLister` on the Windows leg.

// HFRepoFile is one file at the top level of a Hugging Face repository.
type HFRepoFile struct {
	Name string
	Size int64
}

// HFFileLister reads the top level of a repository. It is an interface so
// the pull path can be driven in tests without a network.
type HFFileLister interface {
	ListTopLevel(ctx context.Context, repo, revision string) ([]HFRepoFile, error)
}

// DefaultHFFileLister reads the Hub's tree API.
//
// Not internal/catalog/hfclient, which is the other read-only Hub client in
// this repo: that one exists for catalog AUTHORING and pulls
// internal/catalog/scoring in with it, and this call is part of shaping a
// weights pull. Its package doc states the separation; this comment is the
// other half of it.
type DefaultHFFileLister struct {
	BaseURL string
	HTTP    *http.Client
}

func (l DefaultHFFileLister) ListTopLevel(ctx context.Context, repo, revision string) ([]HFRepoFile, error) {
	base := l.BaseURL
	if base == "" {
		base = HFHubBaseURL
	}
	if revision == "" {
		revision = "main"
	}
	// The tree endpoint is NOT recursive unless asked, which is the whole
	// point: what comes back is the repository's top level, with a
	// `type` per entry and a size per file.
	url := fmt.Sprintf("%s/api/models/%s/tree/%s", base, repo, revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	cl := l.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: list %s: %w", repo, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("download: list %s: %w", repo, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download: list %s: status %d", repo, resp.StatusCode)
	}
	var entries []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("download: list %s: %w", repo, err)
	}
	out := make([]HFRepoFile, 0, len(entries))
	for _, e := range entries {
		if e.Type != "file" || e.Path == "" || strings.Contains(e.Path, "/") {
			continue
		}
		out = append(out, HFRepoFile{Name: e.Path, Size: e.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// HFHasWeights reports whether a top-level listing contains the weights
// themselves, which is the precondition for narrowing a pull to it.
//
// "The top level" is the right set for every repository the catalog names
// today, and wrong for one that keeps its shards in a subdirectory — an
// `nvfp4/` or `fp8/` build, which is exactly the shelf waired-agent#575 is
// filling in. Narrowing there would fetch the config and the tokenizer,
// report 100%, and leave vLLM to fail on a model with no weights. So the
// caller checks, and falls back to the whole repository when the answer is
// no — the same fallback a listing that cannot be read takes.
func HFHasWeights(files []HFRepoFile) bool {
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Name, ".safetensors"),
			strings.HasSuffix(f.Name, ".gguf"),
			strings.HasSuffix(f.Name, ".bin"):
			return true
		}
	}
	return false
}

// HFTotalBytes sums a file list.
func HFTotalBytes(files []HFRepoFile) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// HFFileNames projects the names, in listing order.
func HFFileNames(files []HFRepoFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

// hfIncompleteDir is where huggingface_hub parks a partial file while it
// downloads. The finished file appears under --local-dir only once the
// transfer completes, so a directory walk that ignored this would report
// 0 bytes for the whole of a 14 GB shard and then jump to 100%.
const hfIncompleteDir = ".cache/huggingface/download"

// WatchHFLocalDir reports byte progress for an in-flight `hf download` by
// looking at the target directory, and stops when ctx is done.
//
// It exists because the CLI's own output cannot be turned into bytes:
// parseHFProgressLine reads a percentage per file and nothing else, so
// every event it produces is dropped by the aggregator downstream (which
// requires a digest and a total) and a vLLM model download reported 0/0
// for its entire duration (waired-agent#1298).
//
// One Progress event per file, keyed by file name as the digest, with the
// file's listed size as its total — the shape the layer aggregator already
// sums. Call AnnounceHFFiles first: it puts the whole total in place before
// the pull starts, so the figure does not grow as files appear.
func WatchHFLocalDir(ctx context.Context, localDir string, files []HFRepoFile, interval time.Duration, onProgress func(Progress)) {
	if onProgress == nil || len(files) == 0 {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	last := make(map[string]int64, len(files))
	emit := func(f HFRepoFile, done int64, elapsed time.Duration) {
		if done > f.Size {
			done = f.Size
		}
		var rate int64
		if elapsed > 0 && done > last[f.Name] {
			rate = int64(float64(done-last[f.Name]) / elapsed.Seconds())
		}
		last[f.Name] = done
		onProgress(Progress{
			State:       StatePulling,
			Digest:      f.Name,
			Completed:   done,
			Total:       f.Size,
			Percent:     -1,
			BytesPerSec: rate,
		})
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	prev := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			elapsed := now.Sub(prev)
			prev = now
			// One pass, and the in-flight bytes are spent ONCE. Asking
			// per file and taking the largest partial each time credited
			// every not-yet-present file with the same bytes — on a
			// three-shard model that reads as most of the download done
			// with one shard half-written, and it moves BACKWARDS when a
			// shard finishes and the largest remaining partial is
			// smaller.
			inFlight := hfIncompleteBytes(localDir)
			for _, f := range files {
				done, ok := hfFinishedBytes(localDir, f.Name)
				if !ok {
					done = min(inFlight, f.Size)
					inFlight -= done
				}
				emit(f, done, elapsed)
			}
		}
	}
}

// hfFinishedBytes is the size of a file that has fully landed, and
// whether it has. huggingface_hub moves a file into --local-dir only once
// its transfer completes, so a file that is there is a file that is done.
func hfFinishedBytes(localDir, name string) (int64, bool) {
	fi, err := os.Stat(filepath.Join(localDir, name))
	if err != nil || fi.IsDir() {
		return 0, false
	}
	return fi.Size(), true
}

// hfIncompleteBytes is everything currently parked in the download cache:
// the bytes of every file in flight, summed.
//
// Summed rather than attributed, because it cannot be attributed — the
// partials are named after each file's hash, not after the file. The
// caller spends this total across the files that have not landed, in
// listing order, so no byte is counted twice.
func hfIncompleteBytes(localDir string) int64 {
	entries, err := os.ReadDir(filepath.Join(localDir, hfIncompleteDir))
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".incomplete") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total
}

// AnnounceHFFiles reports every file at zero bytes, so a reader has the
// download's full size before the first byte lands.
//
// Separate from WatchHFLocalDir, which runs on its own goroutine: the
// total has to be in place by the time the caller returns, or a surface
// that reads it in between sees a download with no size.
func AnnounceHFFiles(files []HFRepoFile, onProgress func(Progress)) {
	if onProgress == nil {
		return
	}
	for _, f := range files {
		onProgress(Progress{
			State:   StatePulling,
			Digest:  f.Name,
			Total:   f.Size,
			Percent: -1,
		})
	}
}
