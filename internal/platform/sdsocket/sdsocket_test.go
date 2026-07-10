package sdsocket

import (
	"net"
	"testing"
)

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestListenerByNameResolvesCorrectFD(t *testing.T) {
	env := fakeEnv(map[string]string{
		"LISTEN_PID":     "42",
		"LISTEN_FDS":     "3",
		"LISTEN_FDNAMES": "other:claude-proxy:third",
	})
	var openedFD int
	stub := func(fd int, name string) (net.Listener, error) {
		openedFD = fd
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, err
	}
	ln, err := listenerByName("claude-proxy", env, func() int { return 42 }, stub)
	if err != nil {
		t.Fatalf("listenerByName: %v", err)
	}
	if ln == nil {
		t.Fatal("expected a listener for the matching name")
	}
	defer ln.Close()
	// claude-proxy is index 1 → fd 3+1 = 4.
	if openedFD != 4 {
		t.Errorf("opened fd %d, want 4 (listenStart+index)", openedFD)
	}
}

func TestListenerByNamePIDMismatch(t *testing.T) {
	env := fakeEnv(map[string]string{
		"LISTEN_PID":     "999",
		"LISTEN_FDS":     "1",
		"LISTEN_FDNAMES": "claude-proxy",
	})
	called := false
	stub := func(int, string) (net.Listener, error) { called = true; return nil, nil }
	ln, err := listenerByName("claude-proxy", env, func() int { return 42 }, stub)
	if err != nil || ln != nil {
		t.Errorf("PID mismatch should yield (nil, nil), got ln=%v err=%v", ln, err)
	}
	if called {
		t.Error("must not open any fd on PID mismatch")
	}
}

func TestListenerByNameNotActivated(t *testing.T) {
	ln, err := listenerByName("claude-proxy", fakeEnv(nil), func() int { return 42 },
		func(int, string) (net.Listener, error) { return nil, nil })
	if err != nil || ln != nil {
		t.Errorf("no LISTEN_PID should yield (nil, nil), got ln=%v err=%v", ln, err)
	}
}

func TestListenerByNameNameNotFound(t *testing.T) {
	env := fakeEnv(map[string]string{
		"LISTEN_PID":     "42",
		"LISTEN_FDS":     "2",
		"LISTEN_FDNAMES": "alpha:beta",
	})
	ln, err := listenerByName("claude-proxy", env, func() int { return 42 },
		func(int, string) (net.Listener, error) { return nil, nil })
	if err != nil || ln != nil {
		t.Errorf("unmatched name should yield (nil, nil), got ln=%v err=%v", ln, err)
	}
}
