//go:build !windows

package download

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeHFRunner struct {
	calls   []hfCall
	respond func(call hfCall) (lines []string, err error)
}

type hfCall struct {
	binary string
	args   []string
	env    []string
}

func (r *fakeHFRunner) Run(_ context.Context, binary string, args, env []string, onLine func(string)) error {
	c := hfCall{binary: binary, args: append([]string(nil), args...), env: append([]string(nil), env...)}
	r.calls = append(r.calls, c)
	lines, err := r.respond(c)
	for _, l := range lines {
		onLine(l)
	}
	return err
}

func TestHFPuller_PassesEnvAndArgs(t *testing.T) {
	runner := &fakeHFRunner{
		respond: func(hfCall) ([]string, error) {
			return []string{"/tmp/qwen/Qwen3-0.5B"}, nil
		},
	}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Qwen/Qwen3-0.5B-Instruct", HFPullOpts{
		LocalDir:     "/tmp/qwen",
		Revision:     "abc123",
		FastTransfer: true,
	}, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (FastTransfer first attempt should succeed)", len(runner.calls))
	}
	c := runner.calls[0]
	if c.binary != "/venv/bin/huggingface-cli" {
		t.Errorf("binary = %q", c.binary)
	}
	wantArgs := []string{"download", "Qwen/Qwen3-0.5B-Instruct",
		"--local-dir", "/tmp/qwen",
		"--revision", "abc123"}
	if !sliceEq(c.args, wantArgs) {
		t.Errorf("args = %v, want %v", c.args, wantArgs)
	}
	if !contains(c.env, "HF_HUB_ENABLE_HF_TRANSFER=1") {
		t.Errorf("env should include HF_HUB_ENABLE_HF_TRANSFER=1, got %v", c.env)
	}
}

func TestHFPuller_AutoFallback_DisablesHFTransferOnFailure(t *testing.T) {
	attempt := 0
	runner := &fakeHFRunner{
		respond: func(c hfCall) ([]string, error) {
			attempt++
			if attempt == 1 {
				// First attempt (HF_HUB_ENABLE_HF_TRANSFER=1) fails with
				// what looks like a transport problem.
				return []string{
					"Downloading shards: 12%|█▌ | 1/9",
					"hf_transfer: connection reset by peer",
				}, errors.New("exit status 1")
			}
			// Second attempt (HF_HUB_ENABLE_HF_TRANSFER=0) succeeds.
			return []string{"/tmp/qwen"}, nil
		},
	}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Qwen/X", HFPullOpts{LocalDir: "/tmp/qwen", FastTransfer: true}, nil)
	if err != nil {
		t.Fatalf("Pull (with fallback): %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one attempt + one fallback)", len(runner.calls))
	}
	if !contains(runner.calls[0].env, "HF_HUB_ENABLE_HF_TRANSFER=1") {
		t.Errorf("first env missing HF_HUB_ENABLE_HF_TRANSFER=1: %v", runner.calls[0].env)
	}
	if !contains(runner.calls[1].env, "HF_HUB_ENABLE_HF_TRANSFER=0") {
		t.Errorf("second env missing HF_HUB_ENABLE_HF_TRANSFER=0: %v", runner.calls[1].env)
	}
}

func TestHFPuller_AuthErrorShortCircuits(t *testing.T) {
	runner := &fakeHFRunner{
		respond: func(hfCall) ([]string, error) {
			return []string{"401 Client Error: Unauthorized for url: https://huggingface.co/api/models/Foo/Bar"}, errors.New("exit status 1")
		},
	}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Foo/Bar", HFPullOpts{LocalDir: "/tmp/x", FastTransfer: true}, nil)
	if err == nil {
		t.Fatalf("expected auth error")
	}
	if len(runner.calls) != 1 {
		t.Errorf("auth error must NOT trigger hf_transfer fallback, got %d calls", len(runner.calls))
	}
	var hfErr *HFError
	if !errors.As(err, &hfErr) || hfErr.Class != HFErrAuth {
		t.Errorf("err = %v, want HFErrAuth class", err)
	}
}

func TestHFPuller_NotFoundShortCircuits(t *testing.T) {
	runner := &fakeHFRunner{
		respond: func(hfCall) ([]string, error) {
			return []string{"Repository not found: Qwen/Bogus does not exist"}, errors.New("exit status 1")
		},
	}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Qwen/Bogus", HFPullOpts{LocalDir: "/tmp/x", FastTransfer: true}, nil)
	if err == nil {
		t.Fatalf("expected not-found error")
	}
	if len(runner.calls) != 1 {
		t.Errorf("not-found must NOT trigger fallback, got %d calls", len(runner.calls))
	}
	var hfErr *HFError
	if !errors.As(err, &hfErr) || hfErr.Class != HFErrNotFound {
		t.Errorf("err = %v, want HFErrNotFound", err)
	}
}

func TestHFPuller_TokenInjectsHFTOKEN(t *testing.T) {
	runner := &fakeHFRunner{
		respond: func(hfCall) ([]string, error) { return []string{"/tmp"}, nil },
	}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Qwen/X", HFPullOpts{
		LocalDir: "/tmp", FastTransfer: true, Token: "hf_secret_xyz",
	}, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !contains(runner.calls[0].env, "HF_TOKEN=hf_secret_xyz") {
		t.Errorf("HF_TOKEN should be passed via env, got %v", runner.calls[0].env)
	}
}

func TestHFPuller_LocalDirRequired(t *testing.T) {
	runner := &fakeHFRunner{respond: func(hfCall) ([]string, error) { return nil, nil }}
	p := NewHFPuller("/venv/bin/huggingface-cli", runner)
	err := p.Pull(context.Background(), "Qwen/X", HFPullOpts{}, nil)
	if err == nil || !strings.Contains(err.Error(), "LocalDir") {
		t.Errorf("expected LocalDir-required error, got %v", err)
	}
}

func TestParseHFProgressLine(t *testing.T) {
	cases := []struct {
		line        string
		wantState   string
		wantPercent int
	}{
		{"Downloading shards: 47%|███▌      | 7/9 [02:13<02:08]", StatePulling, 47},
		{"Downloading model.safetensors: 13%|█▎ | 1.4G/10.6G", StatePulling, 13},
		{"Verifying integrity...", StateVerifying, -1},
		{"Computing checksums for 9 files", StateVerifying, -1},
		{"/home/user/.local/share/waired/models/qwen3-32b/awq", StateSuccess, 100},
		{"some unrelated noise", StateUnknown, -1},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			p := parseHFProgressLine(c.line)
			if p.State != c.wantState {
				t.Errorf("State = %q, want %q (line=%q)", p.State, c.wantState, c.line)
			}
			if p.Percent != c.wantPercent {
				t.Errorf("Percent = %d, want %d (line=%q)", p.Percent, c.wantPercent, c.line)
			}
		})
	}
}

func TestEstimateRequiredBytes_HasHeadroom(t *testing.T) {
	// 22 GiB * 1.2 headroom = 26.4 GiB, rounded down for int64.
	got := EstimateRequiredBytes(22.0)
	const wantApprox = int64(28346784153)
	// Allow ±2 bytes for float rounding across compilers.
	if got < wantApprox-2 || got > wantApprox+2 {
		t.Errorf("EstimateRequiredBytes(22) = %d, want ~%d (20%% headroom)", got, wantApprox)
	}
	// Sanity: 22 GB without headroom would be 23622320128. The
	// estimate must be larger than that.
	if got <= 22*1024*1024*1024 {
		t.Errorf("EstimateRequiredBytes(22) = %d, expected larger than raw 22 GiB (%d)", got, int64(22*1024*1024*1024))
	}
}

func TestCheckDiskSpace(t *testing.T) {
	if err := CheckDiskSpace(100, 50); err != nil {
		t.Errorf("100 free vs 50 needed should pass, got %v", err)
	}
	if err := CheckDiskSpace(50, 100); err == nil {
		t.Errorf("50 free vs 100 needed should fail")
	}
	if err := CheckDiskSpace(0, 0); err != nil {
		t.Errorf("zero required should pass even with zero free, got %v", err)
	}
}

func TestResolveHFCLI_Override(t *testing.T) {
	got, err := ResolveHFCLI("/venv/bin/huggingface-cli")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/venv/bin/huggingface-cli" {
		t.Errorf("override path lost: %q", got)
	}
}

func sliceEq(a, b []string) bool {
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

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
