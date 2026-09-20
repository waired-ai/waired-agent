package download

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog/gguf"
)

// ggufWithContextLength writes a minimal GGUF the reader accepts, carrying
// general.architecture and <arch>.context_length, plus some trailing bytes
// standing in for tensor data.
func ggufWithContextLength(t *testing.T, path, arch string, ctx uint32) {
	t.Helper()
	var b []byte
	str := func(s string) {
		b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
		b = append(b, s...)
	}
	b = append(b, "GGUF"...)
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint64(b, 0) // tensor count
	b = binary.LittleEndian.AppendUint64(b, 2) // kv count
	str("general.architecture")
	b = binary.LittleEndian.AppendUint32(b, 8) // string
	str(arch)
	str(arch + ".context_length")
	b = binary.LittleEndian.AppendUint32(b, 4) // uint32
	b = binary.LittleEndian.AppendUint32(b, ctx)
	b = append(b, make([]byte, 1024)...)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// store lays out a model store the way ollama does, with one tag whose
// weights blob is a GGUF claiming ctx.
func storeWithTag(t *testing.T, tag, arch string, ctx uint32) (dir, blob string) {
	t.Helper()
	dir = t.TempDir()
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	writeManifest(t, dir, "registry.ollama.ai/library/"+strings.SplitN(tag, ":", 2)[0]+"/"+strings.SplitN(tag, ":", 2)[1],
		[]map[string]any{{"mediaType": mediaTypeModel, "digest": digest}})
	blob = filepath.Join(dir, "blobs", "sha256-1111111111111111111111111111111111111111111111111111111111111111")
	ggufWithContextLength(t, blob, arch, ctx)
	return dir, blob
}

// A pull that asks for the long window leaves the file able to serve it.
// ollama clamps num_ctx to this value, so without the rewrite the engine
// silently serves 262,144 whatever the product asked for.
func TestPull_RaisesContextLength(t *testing.T) {
	dir, blob := storeWithTag(t, "qwen3.6:35b", "qwen35moe", 262144)
	var logged []string
	p := NewPuller("/bin/ollama", &recordingRunner{}).WithModelStore(dir).
		WithLogger(func(f string, a ...any) { logged = append(logged, f) })

	if err := p.Pull(context.Background(), "qwen3.6:35b", Rendering{ContextLength: 1048576}, nil); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	got, ok, err := gguf.ArchUint32(blob, "context_length")
	if err != nil || !ok {
		t.Fatalf("ArchUint32: %v ok=%v", err, ok)
	}
	if got != 1048576 {
		t.Errorf("context_length = %d, want 1048576", got)
	}
	if len(logged) != 1 {
		t.Errorf("the store edit was not recorded: %v", logged)
	}
}

// Every other pull must leave the file exactly as published. This is the
// test that fails if the rewrite is ever made unconditional.
func TestPull_LeavesContextLengthAloneByDefault(t *testing.T) {
	dir, blob := storeWithTag(t, "qwen3.6:35b", "qwen35moe", 262144)
	before, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller("/bin/ollama", &recordingRunner{}).WithModelStore(dir)

	for _, want := range []Rendering{{}, {Renderer: "qwen3.8"}, {DraftNumPredict: 3}} {
		if err := p.Pull(context.Background(), "qwen3.6:35b", want, nil); err != nil {
			t.Fatalf("Pull(%+v): %v", want, err)
		}
	}
	after, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the weights file changed on a pull that did not ask for a longer window")
	}
}

// The rewrite has to be re-applied on every pull, for the same reason the
// stamp does: a blob that was deleted and re-fetched comes back as the
// publisher wrote it.
func TestPull_RaisesContextLengthOnEveryPull(t *testing.T) {
	dir, blob := storeWithTag(t, "qwen3.6:35b", "qwen35moe", 262144)
	p := NewPuller("/bin/ollama", &recordingRunner{}).WithModelStore(dir)
	want := Rendering{ContextLength: 1048576}

	if err := p.Pull(context.Background(), "qwen3.6:35b", want, nil); err != nil {
		t.Fatal(err)
	}
	// Stand in for a re-fetch: the publisher's value is back.
	ggufWithContextLength(t, blob, "qwen35moe", 262144)
	if err := p.Pull(context.Background(), "qwen3.6:35b", want, nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := gguf.ArchUint32(blob, "context_length")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1048576 {
		t.Errorf("context_length = %d after the second pull, want 1048576", got)
	}
}

// Asking for the long window with no store configured must fail loudly. The
// alternative is a host that believes it serves 1M and serves 262,144.
func TestPull_ContextLengthWithoutAStore(t *testing.T) {
	p := NewPuller("/bin/ollama", &recordingRunner{})
	err := p.Pull(context.Background(), "qwen3.6:35b", Rendering{ContextLength: 1048576}, nil)
	if err == nil {
		t.Fatal("Pull succeeded with no model store")
	}
	if !strings.Contains(err.Error(), "WithModelStore") {
		t.Errorf("error does not name the missing wiring: %v", err)
	}
}

// A build whose file already claims the window costs a read and no write.
func TestPull_ContextLengthAlreadyHighEnough(t *testing.T) {
	dir, blob := storeWithTag(t, "qwen3.6:35b", "qwen35moe", 1048576)
	info, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	p := NewPuller("/bin/ollama", &recordingRunner{}).WithModelStore(dir).
		WithLogger(func(f string, a ...any) { logged = append(logged, f) })
	if err := p.Pull(context.Background(), "qwen3.6:35b", Rendering{ContextLength: 1048576}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("the file was rewritten when it already claimed the window")
	}
	if len(logged) != 0 {
		t.Errorf("a no-op was recorded as an edit: %v", logged)
	}
}

// A failed pull must not reach the file at all.
func TestPull_NoRewriteWhenThePullFails(t *testing.T) {
	dir, blob := storeWithTag(t, "qwen3.6:35b", "qwen35moe", 262144)
	r := &recordingRunner{err: os.ErrPermission, failOn: "pull"}
	p := NewPuller("/bin/ollama", r).WithModelStore(dir)
	if err := p.Pull(context.Background(), "qwen3.6:35b", Rendering{ContextLength: 1048576}, nil); err == nil {
		t.Fatal("Pull succeeded although the runner failed")
	}
	got, _, err := gguf.ArchUint32(blob, "context_length")
	if err != nil {
		t.Fatal(err)
	}
	if got != 262144 {
		t.Errorf("context_length = %d after a failed pull, want it untouched at 262144", got)
	}
}
