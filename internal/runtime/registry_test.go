package runtime

import (
	"context"
	"testing"
)

type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                            { return s.name }
func (s stubAdapter) EnsureRunning(ctx context.Context) error { return nil }
func (s stubAdapter) Health(ctx context.Context) Health       { return Health{State: StateReady} }
func (s stubAdapter) Stop(ctx context.Context) error          { return nil }
func (s stubAdapter) BaseURL() string                         { return "http://stub" }

func TestRegistry_RegisterLookup(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("ollama"); ok {
		t.Errorf("empty registry should miss")
	}
	r.Register(stubAdapter{name: "ollama"})
	if a, ok := r.Lookup("ollama"); !ok || a.Name() != "ollama" {
		t.Errorf("Lookup after Register: a=%v ok=%v", a, ok)
	}
}

func TestRegistry_RegisterNilIgnored(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)
	if names := r.Names(); len(names) != 0 {
		t.Errorf("nil register added an entry: %v", names)
	}
}

func TestRegistry_Names(t *testing.T) {
	r := NewRegistry()
	r.Register(stubAdapter{name: "a"})
	r.Register(stubAdapter{name: "b"})
	got := r.Names()
	if len(got) != 2 {
		t.Errorf("Names = %v, want length 2", got)
	}
}

func TestRegistry_MustLookupPanicsOnMiss(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("MustLookup should panic on miss")
		}
	}()
	NewRegistry().MustLookup("missing")
}
