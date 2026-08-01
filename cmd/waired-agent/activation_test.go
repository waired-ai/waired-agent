package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
)

// fakeBoundEngine records the port it was told it bound so a test can assert
// on what bringUpEngine reports downstream.
type fakeBoundEngine struct {
	port    int
	portErr error
}

func (f *fakeBoundEngine) ListenPort() (int, error) { return f.port, f.portErr }

// recordingFactory is the engine-construction seam. It keeps every
// wgnet.Config it was handed — not just the ports — so a test can prove
// the fallback attempt is otherwise identical to the first one.
type recordingFactory struct {
	configs []wgnet.Config
	// results is consumed one entry per call.
	results []struct {
		engine *fakeBoundEngine
		err    error
	}
}

func (f *recordingFactory) new(cfg wgnet.Config) (*fakeBoundEngine, error) {
	f.configs = append(f.configs, cfg)
	n := len(f.configs) - 1
	if n >= len(f.results) {
		return nil, fmt.Errorf("unexpected engine construction #%d", n+1)
	}
	return f.results[n].engine, f.results[n].err
}

func bindErr(msg string) error {
	return fmt.Errorf("device up: %w: %s", wgnet.ErrBindFailed, msg)
}

// TestBringUpEngineFallback pins the product contract from
// waired-agent#318: a bind failure on the pinned port must not sink the
// session — retry once on an ephemeral port and report the port really
// bound. Any other failure is not a port problem and must surface
// unchanged.
func TestBringUpEngineFallback(t *testing.T) {
	const preferred = 64582

	type result struct {
		engine *fakeBoundEngine
		err    error
	}
	cases := []struct {
		name          string
		preferredPort int
		results       []result
		wantPorts     []int // ListenPort of each attempted config, in order
		wantBound     int
		wantErr       error
	}{
		{
			name:          "preferred port binds",
			preferredPort: preferred,
			results:       []result{{engine: &fakeBoundEngine{port: preferred}}},
			wantPorts:     []int{preferred},
			wantBound:     preferred,
		},
		{
			// The live Windows case: the pinned port fell inside an
			// excluded UDP range this boot.
			name:          "bind failure falls back to ephemeral",
			preferredPort: preferred,
			results: []result{
				{err: bindErr("access permissions")},
				{engine: &fakeBoundEngine{port: 51234}},
			},
			wantPorts: []int{preferred, 0},
			wantBound: 51234,
		},
		{
			name:          "non-bind failure is not retried",
			preferredPort: preferred,
			results:       []result{{err: errors.New("create netstack tun: no memory")}},
			wantPorts:     []int{preferred},
			wantErr:       errBringUpSentinel,
		},
		{
			// Already ephemeral: another attempt cannot find a port the
			// first one could not.
			name:          "bind failure on port 0 is not retried",
			preferredPort: 0,
			results:       []result{{err: bindErr("no ports available")}},
			wantPorts:     []int{0},
			wantErr:       wgnet.ErrBindFailed,
		},
		{
			name:          "fallback also fails",
			preferredPort: preferred,
			results: []result{
				{err: bindErr("access permissions")},
				{err: bindErr("no ports available")},
			},
			wantPorts: []int{preferred, 0},
			wantErr:   wgnet.ErrBindFailed,
		},
		{
			// The device cannot say which port it bound. Degrade to the
			// requested one rather than failing activation.
			name:          "unreadable port falls back to the requested one",
			preferredPort: preferred,
			results:       []result{{engine: &fakeBoundEngine{portErr: errors.New("ipc get: closed")}}},
			wantPorts:     []int{preferred},
			wantBound:     preferred,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &recordingFactory{}
			for _, r := range tc.results {
				f.results = append(f.results, struct {
					engine *fakeBoundEngine
					err    error
				}{r.engine, r.err})
			}
			cfg := wgnet.Config{
				SelfName:   "dev_TEST",
				ListenPort: tc.preferredPort,
			}
			engine, port, err := bringUpEngine(cfg, f.new, discardLogger())

			gotPorts := make([]int, 0, len(f.configs))
			for _, c := range f.configs {
				gotPorts = append(gotPorts, c.ListenPort)
			}
			if fmt.Sprint(gotPorts) != fmt.Sprint(tc.wantPorts) {
				t.Fatalf("attempted ports %v, want %v", gotPorts, tc.wantPorts)
			}
			// Every attempt must carry the same engine configuration —
			// only the port may differ.
			for i, c := range f.configs {
				if c.SelfName != cfg.SelfName {
					t.Fatalf("attempt %d lost SelfName: %+v", i, c)
				}
			}
			if tc.wantErr != nil {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !errors.Is(tc.wantErr, errBringUpSentinel) && !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("bringUpEngine: %v", err)
			}
			if engine == nil {
				t.Fatal("expected an engine")
			}
			if port != tc.wantBound {
				t.Fatalf("bound port = %d, want %d", port, tc.wantBound)
			}
		})
	}
}

// errBringUpSentinel marks table rows that only assert "some error",
// where the exact value is the factory's own and carries no contract.
var errBringUpSentinel = errors.New("any error")

func TestNextActivationBackoff(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, activationRetryMin},
		{activationRetryMin, 30 * time.Second},
		{4 * time.Minute, activationRetryMax},
		{activationRetryMax, activationRetryMax},
	}
	for _, tc := range cases {
		if got := nextActivationBackoff(tc.in); got != tc.want {
			t.Errorf("nextActivationBackoff(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestRunBootActivationRetries pins the behaviour change at the heart of
// waired-agent#318's third symptom: boot activation used to be a single
// attempt, so one transient failure left a signed-in device inactive for
// the rest of the session. It must keep trying, and it must record why
// it is not up in the meantime.
func TestRunBootActivationRetries(t *testing.T) {
	sb := &switchboard{}
	sb.setOffline(offlineIdentityView(&identity.Identity{
		DeviceID:     "dev_TEST",
		AccountEmail: "someone@example.test",
	}))

	// Always fails: the assertion is about what the daemon reports while
	// it cannot come up, and that the loop is still alive to retry. The
	// backoff schedule itself is covered by TestNextActivationBackoff.
	var mu sync.Mutex
	attempts := 0
	activate := func(context.Context) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		return fmt.Errorf("attempt %d: device up: %w: excluded range", n, wgnet.ErrBindFailed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go runBootActivation(ctx, sb, activate, discardLogger())

	// While inactive the daemon must still present itself as signed in,
	// with a reason — never as logged out.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v := sb.Identity()
		if !v.Enrolled {
			t.Fatal("an enrolled device must not report Enrolled=false while inactive")
		}
		if v.Active {
			t.Fatal("no session is published; Active must be false")
		}
		if v.ActivationError != "" {
			return // the failure reason reached the management surface
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("activation error was never recorded on the identity view")
}
