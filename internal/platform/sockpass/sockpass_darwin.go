// Package sockpass passes a listening socket fd from a privileged process to
// an unprivileged one over a unix-domain socket via SCM_RIGHTS.
//
// On macOS the waired-agent runs as a per-user LaunchAgent and cannot bind the
// privileged port 443 the transparent Claude proxy needs. A root LaunchDaemon
// (the "socket-holder broker") binds 127.0.0.1:443 and hands the listening fd
// to the agent through this package — the macOS analog of systemd socket
// activation on Linux (internal/platform/sdsocket). The broker keeps holding
// the socket across agent restarts, so there is never a dead-port window.
//
// The broker side calls Send; the agent side calls Receive. The wire format is
// a single payload byte plus the SCM_RIGHTS ancillary carrying exactly one fd
// (a zero-length payload alongside SCM_RIGHTS is unreliable across platforms).
package sockpass

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Receive dials the unix socket at sockPath and returns the listening
// net.Listener the broker passes over it. It returns (nil, nil) when sockPath
// does not exist — i.e. the broker is not installed, so the proxy is not active
// on this host — mirroring sdsocket.ListenerByName's "not socket-activated"
// contract so the agent treats it as "proxy off" rather than an error.
func Receive(sockPath string) (net.Listener, error) {
	if _, err := os.Stat(sockPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sockpass: stat %s: %w", sockPath, err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("sockpass: dial %s: %w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("sockpass: %s did not yield a *net.UnixConn", sockPath)
	}
	return recvListener(uc)
}

// recvListener reads one SCM_RIGHTS message off uc and wraps the received fd as
// a net.Listener. Split out so a unit test can drive it over a socketpair
// without a real on-disk socket.
func recvListener(uc *net.UnixConn) (net.Listener, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4)) // exactly one fd (int32)

	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("sockpass: syscallconn: %w", err)
	}
	var n, oobn int
	var rerr error
	if cerr := raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
		// Tell the runtime to wait for readiness on EAGAIN; otherwise we're done.
		return rerr != unix.EAGAIN
	}); cerr != nil {
		return nil, fmt.Errorf("sockpass: recvmsg control: %w", cerr)
	}
	if rerr != nil {
		return nil, fmt.Errorf("sockpass: recvmsg: %w", rerr)
	}
	if n < 1 {
		return nil, fmt.Errorf("sockpass: short read (n=%d): broker sent no payload", n)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("sockpass: parse control message: %w", err)
	}
	if len(scms) == 0 {
		return nil, fmt.Errorf("sockpass: no control message (no fd passed)")
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return nil, fmt.Errorf("sockpass: parse unix rights: %w", err)
	}
	if len(fds) == 0 {
		return nil, fmt.Errorf("sockpass: control message carried no fd")
	}
	// Defensive: close any extra fds so a misbehaving sender cannot leak them.
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}

	f := os.NewFile(uintptr(fds[0]), "claude-proxy")
	if f == nil {
		return nil, fmt.Errorf("sockpass: received invalid fd %d", fds[0])
	}
	// net.FileListener dups the fd; close our os.File copy so the fd is not
	// leaked into child processes (same rationale as sdsocket.fdListener).
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("sockpass: received fd is not a listening socket: %w", err)
	}
	return ln, nil
}

// Send passes ln's underlying listening socket fd to the peer on uc via
// SCM_RIGHTS. ln must be a *net.TCPListener (the broker's :443 listener). The
// caller keeps ownership of ln — Send dups the fd (via TCPListener.File) and
// closes only its own copy, so the broker can keep holding the socket and serve
// further agent reconnects.
func Send(uc *net.UnixConn, ln net.Listener) error {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("sockpass: Send requires *net.TCPListener, got %T", ln)
	}
	f, err := tl.File() // dups the listening fd
	if err != nil {
		return fmt.Errorf("sockpass: listener file: %w", err)
	}
	defer func() { _ = f.Close() }()
	rights := unix.UnixRights(int(f.Fd()))

	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("sockpass: syscallconn: %w", err)
	}
	var werr error
	if cerr := raw.Write(func(fd uintptr) bool {
		_, werr = unix.SendmsgN(int(fd), []byte{1}, rights, nil, 0)
		return werr != unix.EAGAIN
	}); cerr != nil {
		return fmt.Errorf("sockpass: sendmsg control: %w", cerr)
	}
	if werr != nil {
		return fmt.Errorf("sockpass: sendmsg: %w", werr)
	}
	return nil
}
