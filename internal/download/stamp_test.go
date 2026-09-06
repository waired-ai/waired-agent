package download

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// A tag whose publisher left the renderer unset renders prompts with the
// GGUF's embedded Jinja, which refuses three of the six shapes a coding
// agent sends (waired-agent#1192). The Modelfile is the whole fix, so its
// exact text is the thing worth pinning.
func TestStamp_WritesTheModelfileAndCreatesInPlace(t *testing.T) {
	var seen string
	r := &recordingRunner{onArgs: func(args []string) {
		// -f names a real file; read it before the runner returns and the
		// temp dir is swept.
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				b, err := os.ReadFile(args[i+1])
				if err != nil {
					t.Errorf("Modelfile unreadable while create was running: %v", err)
					return
				}
				seen = string(b)
			}
		}
	}}
	p := NewPuller("/bin/ollama", r)

	const tag = "ns/model:q2"
	if err := p.Pull(context.Background(), tag, Rendering{Renderer: "qwen3.8", Parser: "qwen3.5"}, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Same name in, same name out: a derived name would have to be threaded
	// through every caller that already holds the tag.
	if len(r.seen) != 2 || r.seen[0][0] != "pull" {
		t.Fatalf("commands = %v, want a pull then a create", r.seen)
	}
	wantArgs := []string{"create", tag, "-f"}
	for i, w := range wantArgs {
		if i >= len(r.seen[1]) || r.seen[1][i] != w {
			t.Fatalf("create argv = %v, want it to start %v", r.seen[1], wantArgs)
		}
	}
	want := "FROM ns/model:q2\nRENDERER qwen3.8\nPARSER qwen3.5\n"
	if seen != want {
		t.Errorf("Modelfile =\n%q\nwant\n%q", seen, want)
	}
}

// An empty declaration is the common case — nearly every tag ships with a
// renderer already — and must cost nothing beyond the pull itself.
func TestStamp_IsANoOpWithoutARenderer(t *testing.T) {
	r := &recordingRunner{}
	p := NewPuller("/bin/ollama", r)
	if err := p.Pull(context.Background(), "ns/model:q4", Rendering{}, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if r.calls != 1 {
		t.Errorf("ran %d commands for an empty renderer; want 1 (the pull alone): %v",
			r.calls, r.seen)
	}
	if got := r.seen[0][0]; got != "pull" {
		t.Errorf("first command was %q, want pull", got)
	}
}

// Either half alone is legal: a tag can need a response parser without a
// prompt renderer, and the Modelfile must then carry only the line it has.
func TestStamp_OmitsTheLineItWasNotGiven(t *testing.T) {
	var seen string
	r := &recordingRunner{onArgs: func(args []string) {
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				b, _ := os.ReadFile(args[i+1])
				seen = string(b)
			}
		}
	}}
	p := NewPuller("/bin/ollama", r)
	if err := p.Pull(context.Background(), "ns/m:t", Rendering{Renderer: "qwen3.8"}, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if strings.Contains(seen, "PARSER") {
		t.Errorf("Modelfile carries a PARSER line that was never asked for:\n%q", seen)
	}
	if !strings.Contains(seen, "RENDERER qwen3.8") {
		t.Errorf("Modelfile lost the renderer it was given:\n%q", seen)
	}
}

// The error must name the tag and the renderer: a bare `ollama create`
// failure in a log says nothing about which model stopped being servable.
func TestStamp_ErrorNamesTheTagAndRenderer(t *testing.T) {
	// Fail the create and only the create: a pull that failed would be
	// reported verbatim, and this is about the stamp's own error text.
	r := &recordingRunner{failOn: "create", err: errors.New("boom")}
	p := NewPuller("/bin/ollama", r)
	err := p.Pull(context.Background(), "ns/model:q2", Rendering{Renderer: "qwen3.8", Parser: "qwen3.5"}, nil)
	if err == nil {
		t.Fatal("Pull succeeded on a failing stamp")
	}
	for _, want := range []string{"ns/model:q2", "qwen3.8", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The temp Modelfile is the only thing Stamp leaves on disk, and it must
// not survive the call.
func TestStamp_LeavesNoModelfileBehind(t *testing.T) {
	var path string
	r := &recordingRunner{onArgs: func(args []string) {
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				path = args[i+1]
			}
		}
	}}
	p := NewPuller("/bin/ollama", r)
	if err := p.Pull(context.Background(), "ns/m:t", Rendering{Renderer: "qwen3.8"}, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if path == "" {
		t.Fatal("create was never given a -f path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists after Pull returned (stat err %v)", path, err)
	}
}

type recordingRunner struct {
	args   []string
	seen   [][]string
	calls  int
	err    error
	failOn string // subcommand to fail on; empty fails every call
	onArgs func([]string)
}

func (r *recordingRunner) Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error {
	r.calls++
	r.args = args
	r.seen = append(r.seen, args)
	if r.onArgs != nil {
		r.onArgs(args)
	}
	if r.err != nil && (r.failOn == "" || (len(args) > 0 && args[0] == r.failOn)) {
		return r.err
	}
	return nil
}

// The stamp must survive a re-pull, and only Pull can guarantee that.
//
// `ollama pull` on a tag that is already present rewrites the local
// manifest back to the published config, clearing the renderer — measured
// at 2 s and a 0.00 GB disk delta on an already-stamped 78.87 GB tag. So
// the second pull has to re-stamp, or the model quietly goes back to
// refusing three of the six shapes (waired-agent#1192).
func TestPull_ReStampsOnEveryPull(t *testing.T) {
	r := &recordingRunner{}
	p := NewPuller("/bin/ollama", r)
	want := Rendering{Renderer: "qwen3.8", Parser: "qwen3.5"}

	for i := range 2 {
		if err := p.Pull(context.Background(), "ns/m:t", want, nil); err != nil {
			t.Fatalf("pull %d: %v", i+1, err)
		}
	}

	var creates int
	for _, args := range r.seen {
		if len(args) > 0 && args[0] == "create" {
			creates++
		}
	}
	if creates != 2 {
		t.Errorf("two pulls produced %d creates, want 2: %v", creates, r.seen)
	}
}

// A pull that failed is reported verbatim and nothing is stamped onto a
// model that may not be there.
func TestPull_DoesNotStampAfterAFailedPull(t *testing.T) {
	r := &recordingRunner{failOn: "pull", err: errors.New("network went away")}
	p := NewPuller("/bin/ollama", r)

	err := p.Pull(context.Background(), "ns/m:t", Rendering{Renderer: "qwen3.8"}, nil)
	if err == nil || !strings.Contains(err.Error(), "network went away") {
		t.Fatalf("error = %v, want the runner's own text", err)
	}
	for _, args := range r.seen {
		if len(args) > 0 && args[0] == "create" {
			t.Errorf("stamped after a failed pull: %v", r.seen)
		}
	}
}
