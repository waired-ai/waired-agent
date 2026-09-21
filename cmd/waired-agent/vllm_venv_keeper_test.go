package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeVenvs is the installer the keeper reads and prunes through: `current`
// is a field the test moves, as a converge does, and every prune records the
// in-use set it was handed.
type fakeVenvs struct {
	mu       sync.Mutex
	current  string
	pruned   []map[string]bool
	remove   []string
	pruneErr error
	lockErr  error
	locks    int
	// pruneEntered is closed when a prune starts; pruneGate, when set,
	// holds it until the test closes it.
	pruneEntered chan struct{}
	pruneGate    chan struct{}
}

func (f *fakeVenvs) keeper() *vllmVenvKeeper {
	return &vllmVenvKeeper{
		active: func() (infruntime.InstallResult, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.current == "" {
				return infruntime.InstallResult{}, false
			}
			return infruntime.InstallResult{Dir: f.current, Version: infruntime.VLLMVersionOfDir(f.current),
				BinDir: "/venvs/" + f.current + "/.venv/bin"}, true
		},
		prune: func(inUse map[string]bool) ([]string, error) {
			if f.pruneEntered != nil {
				close(f.pruneEntered)
			}
			if f.pruneGate != nil {
				<-f.pruneGate
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.pruned = append(f.pruned, inUse)
			return f.remove, f.pruneErr
		},
		lock: func(context.Context) (func(), error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.locks++
			if f.lockErr != nil {
				return nil, f.lockErr
			}
			return func() {}, nil
		},
		refs: map[string]int{},
	}
}

func (f *fakeVenvs) setCurrent(dir string) { f.mu.Lock(); f.current = dir; f.mu.Unlock() }

func keeperLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// The case #1431 is about, at the daemon: an engine started from 0.28.0,
// then a converge made 0.29.0 current. The reclaim that follows the
// converge keeps 0.28.0 and says so; once the engine lets go of it, the
// next reclaim hands the prune an empty in-use set and it goes.
func TestVLLMVenvKeeper_KeepsTheVenvAnEngineRunsFrom(t *testing.T) {
	f := &fakeVenvs{current: "0.28.0"}
	k := f.keeper()
	_, release, ok := k.hold()
	if !ok {
		t.Fatal("hold on an installed host said no venv")
	}
	f.setCurrent("0.29.0") // the converge swapped `current`

	var buf bytes.Buffer
	k.reclaim(context.Background(), keeperLog(&buf))
	if len(f.pruned) != 1 || !f.pruned[0]["0.28.0"] {
		t.Fatalf("prune was handed %v, want 0.28.0 in use", f.pruned)
	}
	if !strings.Contains(buf.String(), "still in use") || !strings.Contains(buf.String(), "0.28.0") {
		t.Errorf("log = %q, want the kept venv named", buf.String())
	}

	release()
	f.remove = []string{"0.28.0"}
	buf.Reset()
	k.reclaim(context.Background(), keeperLog(&buf))
	if len(f.pruned) != 2 || len(f.pruned[1]) != 0 {
		t.Fatalf("second prune was handed %v, want nothing in use", f.pruned)
	}
	if !strings.Contains(buf.String(), "removed the superseded vLLM venv") {
		t.Errorf("log = %q, want the removal reported", buf.String())
	}
}

// Resolving `current` and marking it held is one step against the reclaim:
// a hold that arrives while a prune runs waits for it, so no venv can be
// resolved by one and removed by the other at once.
func TestVLLMVenvKeeper_HoldWaitsForARunningReclaim(t *testing.T) {
	f := &fakeVenvs{current: "0.29.0", pruneEntered: make(chan struct{}), pruneGate: make(chan struct{})}
	k := f.keeper()
	reclaimed := make(chan struct{})
	go func() {
		k.reclaim(context.Background(), keeperLog(&bytes.Buffer{}))
		close(reclaimed)
	}()
	select {
	case <-f.pruneEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reclaim never reached the prune")
	}
	held := make(chan struct{})
	go func() {
		_, release, _ := k.hold()
		release()
		close(held)
	}()
	select {
	case <-held:
		t.Fatal("hold returned while a prune was deciding")
	case <-time.After(50 * time.Millisecond):
	}
	close(f.pruneGate)
	<-reclaimed
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("hold never returned after the prune")
	}
}

func TestVLLMVenvKeeper_Edges(t *testing.T) {
	t.Run("nothing installed", func(t *testing.T) {
		f := &fakeVenvs{}
		k := f.keeper()
		if _, _, ok := k.hold(); ok {
			t.Error("hold said a venv is active on a host with none")
		}
		k.reclaim(context.Background(), keeperLog(&bytes.Buffer{}))
		if f.locks != 0 || len(f.pruned) != 0 {
			t.Errorf("locks=%d prunes=%d on a host with no venv; want neither", f.locks, len(f.pruned))
		}
	})
	t.Run("another install holds the lock", func(t *testing.T) {
		f := &fakeVenvs{current: "0.29.0", lockErr: errors.New("another vLLM install is still running")}
		var buf bytes.Buffer
		f.keeper().reclaim(context.Background(), keeperLog(&buf))
		if len(f.pruned) != 0 {
			t.Error("pruned without the lock")
		}
		if !strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("log = %q, want a WARN saying the reclaim was skipped", buf.String())
		}
	})
	t.Run("release twice", func(t *testing.T) {
		f := &fakeVenvs{current: "0.29.0"}
		k := f.keeper()
		_, first, _ := k.hold()
		_, second, _ := k.hold()
		first()
		first()
		if k.refs["0.29.0"] != 1 {
			t.Errorf("refs = %v after releasing one of two holds twice, want 1", k.refs)
		}
		second()
		if len(k.refs) != 0 {
			t.Errorf("refs = %v after both released, want none", k.refs)
		}
	})
}

// The registered adapter holds the venv it spawns from until a later
// bootstrap replaces it, and the status row reads that venv's release.
func TestProvider_AdapterHoldsItsVenvUntilReplaced(t *testing.T) {
	f := &fakeVenvs{current: "0.28.0"}
	p := &agentInferenceProvider{vllmVenvs: f.keeper()}
	k := p.vllmVenvKeeper()

	v1, rel1, _ := k.hold()
	p.holdVLLMVenvForAdapter(v1.Dir, rel1)
	f.setCurrent("0.29.0~2")
	if got := p.vllmAdapterVenvVersion(); got != "0.28.0" {
		t.Errorf("adapter venv version = %q while the old engine runs, want 0.28.0", got)
	}

	v2, rel2, _ := k.hold()
	p.holdVLLMVenvForAdapter(v2.Dir, rel2)
	if _, still := k.refs["0.28.0"]; still || k.refs["0.29.0~2"] != 1 {
		t.Errorf("refs = %v after the adapter was replaced, want only 0.29.0~2", k.refs)
	}
	if got := p.vllmAdapterVenvVersion(); got != "0.29.0" {
		t.Errorf("adapter venv version = %q, want 0.29.0 (the directory 0.29.0~2)", got)
	}
}
