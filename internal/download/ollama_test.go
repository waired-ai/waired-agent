package download

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestParseProgressLine(t *testing.T) {
	cases := []struct {
		in        string
		wantState string
		wantPct   int
	}{
		{"pulling manifest", StatePulling, -1},
		{"pulling abc123def456: 100% ▕███████▏ 5.0 GB", StatePulling, 100},
		{"pulling abc123def456:   0%                       0 B/5.0 GB", StatePulling, 0},
		{"pulling abc123def456:  45% 2.3 GB/5.0 GB 40 MB/s 62s", StatePulling, 45},
		{"verifying sha256 digest", StateVerifying, -1},
		{"writing manifest", StateVerifying, -1},
		{"removing any unused layers", StateVerifying, -1},
		{"success", StateSuccess, -1},
		{"random unrelated text", StateUnknown, -1},
		{"", StateUnknown, -1},
	}
	for _, c := range cases {
		got := parseProgressLine(c.in)
		if got.State != c.wantState {
			t.Errorf("parseProgressLine(%q).State = %q, want %q", c.in, got.State, c.wantState)
		}
		if got.Percent != c.wantPct {
			t.Errorf("parseProgressLine(%q).Percent = %d, want %d", c.in, got.Percent, c.wantPct)
		}
	}
}

func TestParseProgressLine_Sizes(t *testing.T) {
	cases := []struct {
		in            string
		wantDigest    string
		wantCompleted int64
		wantTotal     int64
		wantSpeed     int64
	}{
		{"pulling manifest", "", 0, 0, 0},
		{"pulling abc123def456: 100% ▕███████▏ 5.0 GB", "abc123def456", 0, 0, 0},
		{"pulling abc123def456:   0%                       0 B/5.0 GB", "abc123def456", 0, 5_000_000_000, 0},
		{"pulling abc123def456:  45% 2.3 GB/5.0 GB 40 MB/s 62s", "abc123def456", 2_300_000_000, 5_000_000_000, 40_000_000},
		{"pulling abc123def456:  10% 100 MiB/1.0 GiB 5 MiB/s", "abc123def456", 104_857_600, 1_073_741_824, 5_242_880},
	}
	for _, c := range cases {
		got := parseProgressLine(c.in)
		if got.Digest != c.wantDigest {
			t.Errorf("parseProgressLine(%q).Digest = %q, want %q", c.in, got.Digest, c.wantDigest)
		}
		if got.Completed != c.wantCompleted {
			t.Errorf("parseProgressLine(%q).Completed = %d, want %d", c.in, got.Completed, c.wantCompleted)
		}
		if got.Total != c.wantTotal {
			t.Errorf("parseProgressLine(%q).Total = %d, want %d", c.in, got.Total, c.wantTotal)
		}
		if got.BytesPerSec != c.wantSpeed {
			t.Errorf("parseProgressLine(%q).BytesPerSec = %d, want %d", c.in, got.BytesPerSec, c.wantSpeed)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2_300_000_000, "2.3 GB"},
		{5_000_000_000, "5.0 GB"},
		{1_500_000, "1.5 MB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.in); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeRunner emits a scripted sequence of stderr lines and then
// returns a configured error from Wait.
type fakeRunner struct {
	lines        []string
	finalErr     error
	calledBinary []string
	calledArg    []string
	calledEnv    []string
}

func (r *fakeRunner) Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error {
	r.calledBinary = append(r.calledBinary, binary)
	r.calledArg = args
	r.calledEnv = env
	for _, line := range r.lines {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		onLine(line)
	}
	return r.finalErr
}

func TestPuller_Success(t *testing.T) {
	r := &fakeRunner{
		lines: []string{
			"pulling manifest",
			"pulling abc123:  10% 0.5 GB/5.0 GB",
			"pulling abc123:  50% 2.5 GB/5.0 GB",
			"pulling abc123: 100% 5.0 GB/5.0 GB",
			"verifying sha256 digest",
			"writing manifest",
			"success",
		},
	}
	puller := NewPuller("ollama", r)
	var mu sync.Mutex
	var events []Progress
	err := puller.Pull(context.Background(), "qwen3:8b-q4_K_M", func(p Progress) {
		mu.Lock()
		events = append(events, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no progress events emitted")
	}
	last := events[len(events)-1]
	if last.State != StateSuccess {
		t.Errorf("last event state = %q, want %q", last.State, StateSuccess)
	}
	// Verify that the mid-stream had at least one downloading event with percent > 0.
	sawPercent := false
	for _, e := range events {
		if e.State == StatePulling && e.Percent >= 50 {
			sawPercent = true
			break
		}
	}
	if !sawPercent {
		t.Errorf("expected to see a downloading event with percent>=50, got %+v", events)
	}
	// Verify the runner was invoked with `pull <tag>`.
	wantArgs := []string{"pull", "qwen3:8b-q4_K_M"}
	if !equal(r.calledArg, wantArgs) {
		t.Errorf("ollama args = %v, want %v", r.calledArg, wantArgs)
	}
}

func TestPuller_Failure(t *testing.T) {
	r := &fakeRunner{
		lines:    []string{"pulling manifest", "Error: model not found"},
		finalErr: errors.New("exit status 1"),
	}
	puller := NewPuller("ollama", r)
	err := puller.Pull(context.Background(), "missing:tag", func(Progress) {})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error should mention runner failure, got: %v", err)
	}
}

func TestPuller_ContextCancel(t *testing.T) {
	r := &fakeRunner{
		lines: []string{"pulling manifest", "pulling abc123: 50%"},
	}
	puller := NewPuller("ollama", r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled
	err := puller.Pull(ctx, "any:tag", func(Progress) {})
	if err == nil {
		t.Errorf("expected context cancellation error, got nil")
	}
}

func TestPuller_NilCallbackOK(t *testing.T) {
	// nil callback must not crash.
	r := &fakeRunner{lines: []string{"pulling manifest", "success"}}
	puller := NewPuller("ollama", r)
	if err := puller.Pull(context.Background(), "any:tag", nil); err != nil {
		t.Errorf("Pull with nil cb: %v", err)
	}
}

// TestPuller_PassesEnv: `ollama pull` is a client of the serving
// engine — the constructor's env (OLLAMA_HOST pointing at the
// waired-owned port) must reach every pull subprocess, or pulls land
// on whatever answers the upstream default 11434.
func TestPuller_PassesEnv(t *testing.T) {
	r := &fakeRunner{lines: []string{"success"}}
	puller := NewPuller("ollama", r, "OLLAMA_HOST=127.0.0.1:9475")
	if err := puller.Pull(context.Background(), "any:tag", nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(r.calledEnv) != 1 || r.calledEnv[0] != "OLLAMA_HOST=127.0.0.1:9475" {
		t.Errorf("runner env = %v, want [OLLAMA_HOST=127.0.0.1:9475]", r.calledEnv)
	}
}

// Product contract (#304): a Puller built with a resolver asks it on
// EVERY pull, so an agent that booted before the engine existed reaches
// a binary that appeared afterwards — including one the $PATH/well-known
// walk in ResolveBinary cannot see, which is where waired's own Linux
// install lands.
func TestPuller_ResolvesTheBinaryOnEveryPull(t *testing.T) {
	r := &fakeRunner{lines: []string{"success"}}
	calls := 0
	puller := NewResolvingPuller(func() (string, error) {
		calls++
		return "/var/lib/waired/runtimes/ollama/bin/ollama", nil
	}, r)

	for i := range 2 {
		if err := puller.Pull(context.Background(), "qwen3:8b-q4_K_M", nil); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Errorf("resolver calls = %d, want 2 (a re-install must be picked up)", calls)
	}
	want := []string{
		"/var/lib/waired/runtimes/ollama/bin/ollama",
		"/var/lib/waired/runtimes/ollama/bin/ollama",
	}
	if !equal(r.calledBinary, want) {
		t.Errorf("runner binary = %v, want %v", r.calledBinary, want)
	}
}

// Product contract: the resolver's error reaches the caller intact. The
// daemon's resolver returns an actionable "run `waired runtimes install
// ollama`" message that a bare ErrNotInstalled would have swallowed.
func TestPuller_ResolverErrorIsSurfaced(t *testing.T) {
	r := &fakeRunner{lines: []string{"success"}}
	sentinel := errors.New("ollama not installed under the state dir")
	puller := NewResolvingPuller(func() (string, error) { return "", sentinel }, r)

	err := puller.Pull(context.Background(), "qwen3:8b-q4_K_M", nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Pull() error = %v, want %v", err, sentinel)
	}
	if len(r.calledBinary) != 0 {
		t.Errorf("runner ran with %v; an unresolved binary must not spawn", r.calledBinary)
	}
}

// Records today's behaviour: NewPuller's explicit path still wins, so the
// callers that were not migrated to a resolver are unaffected.
func TestNewPuller_ExplicitBinaryStillWins(t *testing.T) {
	r := &fakeRunner{lines: []string{"success"}}
	puller := NewPuller("/usr/bin/ollama", r)
	if err := puller.Pull(context.Background(), "qwen3:8b-q4_K_M", nil); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !equal(r.calledBinary, []string{"/usr/bin/ollama"}) {
		t.Errorf("runner binary = %v, want [/usr/bin/ollama]", r.calledBinary)
	}
}

func TestPercentExtractor(t *testing.T) {
	cases := map[string]int{
		"  0% ":           0,
		" 100% ":          100,
		" 45% rest":       45,
		"no percent here": -1,
		"99.9% no":        -1, // we only accept whole-number percents
	}
	for in, want := range cases {
		if got := extractPercent(in); got != want {
			t.Errorf("extractPercent(%q) = %d, want %d", in, got, want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
