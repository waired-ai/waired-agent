package sockpass

import (
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// shortTempDir is t.TempDir() with a path short enough to bind a unix socket
// under. macOS puts TMPDIR at /var/folders/<2>/<30>/T — 48 bytes — and
// t.TempDir() then appends the test name, ten random digits and /001, which
// overruns darwin's 104-byte sockaddr_un.sun_path and makes bind() fail with
// EINVAL. Every site here is fixed, not just the ones that happen to overrun
// today: the rest sit near the boundary and fail as soon as a test name grows.
// Reproducible on Linux with a 52-byte TMPDIR, which leaves the same headroom
// against Linux's 108-byte limit (#216).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// socketpairConns returns a connected pair of *net.UnixConn backed by an
// AF_UNIX SOCK_STREAM socketpair — the in-memory analog of the broker<->agent
// unix socket, so the fd hand-off can be tested without root or a real :443.
func socketpairConns(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	mk := func(fd int, name string) *net.UnixConn {
		f := os.NewFile(uintptr(fd), name)
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("FileConn(%s): %v", name, err)
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			t.Fatalf("FileConn(%s): not *net.UnixConn (%T)", name, c)
		}
		return uc
	}
	return mk(fds[0], "broker"), mk(fds[1], "agent")
}

// TestSendRecvListener is the load-bearing check for Option A: a listening TCP
// socket passed via SCM_RIGHTS must arrive as a usable net.Listener on which a
// dial-to-Accept round-trip completes. This exercises the exact mechanism the
// macOS broker (Send) and agent (recvListener) rely on.
func TestSendRecvListener(t *testing.T) {
	brokerConn, agentConn := socketpairConns(t)
	defer func() { _ = brokerConn.Close() }()
	defer func() { _ = agentConn.Close() }()

	// The broker's :443 stand-in: an ephemeral high port, no privilege needed.
	srcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srcLn.Close() }()

	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(brokerConn, srcLn) }()

	gotLn, err := recvListener(agentConn)
	if err != nil {
		t.Fatalf("recvListener: %v", err)
	}
	defer func() { _ = gotLn.Close() }()
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotLn.Addr().String() != srcLn.Addr().String() {
		t.Fatalf("received listener addr %q != source %q", gotLn.Addr(), srcLn.Addr())
	}

	// A dial to the shared port must be accepted on the RECEIVED listener,
	// proving it is a live dup of the source listening socket.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := gotLn.Accept()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	dialed, err := net.DialTimeout("tcp", gotLn.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial received listener: %v", err)
	}
	defer func() { _ = dialed.Close() }()

	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("accept on received listener failed")
		}
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Accept on received listener")
	}
}

// TestSendRejectsNonTCP guards the broker contract: only a *net.TCPListener
// (the :443 listener) may be passed.
func TestSendRejectsNonTCP(t *testing.T) {
	brokerConn, agentConn := socketpairConns(t)
	defer func() { _ = brokerConn.Close() }()
	defer func() { _ = agentConn.Close() }()

	dir := shortTempDir(t)
	uln, err := net.Listen("unix", dir+"/x.sock")
	if err != nil {
		t.Fatalf("unix listen: %v", err)
	}
	defer func() { _ = uln.Close() }()

	if err := Send(brokerConn, uln); err == nil {
		t.Fatal("Send accepted a non-TCP listener; want error")
	}
}

// TestReceiveMissingSocket confirms the "broker not installed" path returns
// (nil, nil) rather than an error, so the agent reads it as "proxy off".
func TestReceiveMissingSocket(t *testing.T) {
	ln, err := Receive(shortTempDir(t) + "/does-not-exist.sock")
	if err != nil {
		t.Fatalf("Receive(missing) error = %v, want nil", err)
	}
	if ln != nil {
		t.Fatalf("Receive(missing) listener = %v, want nil", ln)
	}
}
