//go:build treeit

package runtime

// A manual check against a real ollama, never compiled by CI (build tag
// treeit). It drives the product's OllamaAdapter and DefaultSpawner through
// a crash of `ollama serve` that leaves its llama-server runner behind, and
// checks that the next engine is spawned only after the runner is gone
// (waired-ai/waired-agent#1443).
//
//	go test -c -tags treeit -o tree.test ./internal/runtime/
//	WAIRED_TREE_IT_OLLAMA=/path/to/ollama WAIRED_TREE_IT_MODELS=/path/to/models \
//	WAIRED_TREE_IT_MODEL=qwen3.5:0.8b ./tree.test -test.run RealEngine -test.v
//
// The model must already be in WAIRED_TREE_IT_MODELS; nothing is pulled.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// checkingSpawner records, at each spawn, whether any process of the
// previously spawned tree was still running.
type checkingSpawner struct {
	inner DefaultSpawner
	mu    sync.Mutex
	prev  RunningProcess
	notes []string
}

func (s *checkingSpawner) Spawn(ctx context.Context, bin string, args, env []string, logW io.Writer) (RunningProcess, error) {
	s.mu.Lock()
	if s.prev != nil {
		alive, err := s.prev.(ProcessTree).TreeAlive()
		s.notes = append(s.notes, fmt.Sprintf("previous tree (pid %d) alive at spawn: %v (err %v)", s.prev.PID(), alive, err))
	}
	s.mu.Unlock()
	p, err := s.inner.Spawn(ctx, bin, args, env, logW)
	s.mu.Lock()
	s.prev = p
	s.mu.Unlock()
	return p, err
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRealEngine_RestartWaitsForTheOrphanedRunner(t *testing.T) {
	bin, models, model := os.Getenv("WAIRED_TREE_IT_OLLAMA"), os.Getenv("WAIRED_TREE_IT_MODELS"), os.Getenv("WAIRED_TREE_IT_MODEL")
	if bin == "" || models == "" || model == "" {
		t.Fatalf("set WAIRED_TREE_IT_OLLAMA, WAIRED_TREE_IT_MODELS and WAIRED_TREE_IT_MODEL")
	}
	port := freePort(t)
	sp := &checkingSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:              bin,
		Host:                "127.0.0.1",
		Port:                port,
		ModelsDir:           models,
		StateHome:           t.TempDir(),
		LogDir:              t.TempDir(),
		Spawner:             sp,
		HealthInterval:      200 * time.Millisecond,
		HealthSuccess:       1,
		StartupReadyTimeout: time.Minute,
		PendingExits:        NewPendingExits(5*time.Minute, 200*time.Millisecond),
	})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Load the model so a runner exists.
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": "hi", "stream": false, "keep_alive": "10m",
		"options": map[string]any{"num_predict": 1}})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/generate", port), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load: HTTP %d", resp.StatusCode)
	}

	sp.mu.Lock()
	leader := sp.prev
	sp.mu.Unlock()
	t.Logf("leader pid %d", leader.PID())

	// Crash the server alone: its runner is orphaned but stays in the tree.
	if p, err := os.FindProcess(leader.PID()); err == nil {
		if err := p.Kill(); err != nil {
			t.Fatalf("kill leader: %v", err)
		}
	}
	<-leader.Done()
	alive, err := leader.(ProcessTree).TreeAlive()
	t.Logf("after killing only the server: tree alive = %v (err %v)", alive, err)
	if !alive {
		t.Fatalf("the runner did not outlive its server; this check has nothing to show")
	}

	// The adapter notices the crash; the next start reaps and must wait.
	deadline := time.Now().Add(30 * time.Second)
	for a.Health(context.Background()).State == StateReady && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	start := time.Now()
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Logf("restart took %s", time.Since(start).Round(time.Millisecond))
	sp.mu.Lock()
	notes := append([]string(nil), sp.notes...)
	sp.mu.Unlock()
	for _, n := range notes {
		t.Log(n)
	}
	if len(notes) != 1 {
		t.Fatalf("spawn notes = %d, want 1", len(notes))
	}
	if want := fmt.Sprintf("previous tree (pid %d) alive at spawn: false", leader.PID()); len(notes[0]) < len(want) || notes[0][:len(want)] != want {
		t.Errorf("the new engine was spawned while the previous tree was alive: %s", notes[0])
	}
}
