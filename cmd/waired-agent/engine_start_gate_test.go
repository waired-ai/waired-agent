package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// engineStartGate holds an ollama start until the start-up converge's
// ollama pass is over (waired-agent#1511), says so once, and lets a Stop end
// the wait.
func TestEngineStartGate(t *testing.T) {
	t.Run("the update is already over", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		var buf bytes.Buffer
		if err := engineStartGate(done, convergeLogger(&buf))(context.Background()); err != nil {
			t.Fatalf("gate = %v, want nil", err)
		}
		if buf.Len() != 0 {
			t.Errorf("log = %q; a start that did not wait must not say it did", buf.String())
		}
	})

	t.Run("a start waits for the update, and says so once", func(t *testing.T) {
		done := make(chan struct{})
		var buf bytes.Buffer
		gate := engineStartGate(done, convergeLogger(&buf))
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- gate(context.Background()) }()
		}
		select {
		case err := <-results:
			t.Fatalf("a start passed the gate before the update was over (err=%v)", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(done)
		for range 2 {
			if err := <-results; err != nil {
				t.Errorf("gate after the update = %v, want nil", err)
			}
		}
		if n := strings.Count(buf.String(), "waiting for the bundled engine update"); n != 1 {
			t.Errorf("the wait was logged %d times, want once: %q", n, buf.String())
		}
	})

	t.Run("a stop ends the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		gate := engineStartGate(make(chan struct{}), convergeLogger(&bytes.Buffer{}))
		errc := make(chan error, 1)
		go func() { errc <- gate(ctx) }()
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("gate after a stop = %v, want context.Canceled", err)
		}
	})
}
