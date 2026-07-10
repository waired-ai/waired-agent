package runtime

import (
	"errors"
	"sync"
)

// Registry is a small in-memory map of "engine name → Adapter" that
// lets the gateway, router, and management handlers discover what
// runtimes the agent has wired up. Phase A only ever holds one entry
// (Ollama) but the registry shape is shared with Phase B (vLLM).
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register installs a (or replaces an existing) adapter under
// adapter.Name(). Registering nil is treated as removal.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a == nil {
		return
	}
	r.adapters[a.Name()] = a
}

// Lookup returns the adapter registered under name and ok=true, or
// the zero adapter and ok=false.
func (r *Registry) Lookup(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Names returns a snapshot of registered runtime names. Order is
// unspecified.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	return out
}

// MustLookup is a convenience for call sites that already know an
// adapter is configured (e.g. management handlers built atop a
// registry seeded by main).
func (r *Registry) MustLookup(name string) Adapter {
	a, ok := r.Lookup(name)
	if !ok {
		panic(errors.New("runtime: no adapter registered for " + name))
	}
	return a
}
