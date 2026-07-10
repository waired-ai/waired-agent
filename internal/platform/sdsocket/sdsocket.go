// Package sdsocket reads listeners passed by a socket-activating service
// manager (systemd on Linux; launchd uses a different API and is handled
// elsewhere) via the sd_listen_fds protocol:
//
//	LISTEN_PID      must equal our PID (the fds are for this process)
//	LISTEN_FDS      number of inherited fds, starting at fd 3
//	LISTEN_FDNAMES  ':'-separated names, one per fd (set by the .socket
//	                unit's FileDescriptorName=)
//
// waired uses this so the unprivileged agent (User=waired,
// NoNewPrivileges=yes) can serve the Claude proxy on the privileged port
// 443 without CAP_NET_BIND_SERVICE: systemd (root) binds 127.0.0.1:443 in a
// .socket unit and passes the fd here. systemd holds the socket across agent
// restarts, so there is never a dead-port window.
//
// The protocol is just environment parsing + net.FileListener, so this file
// is OS-agnostic; on a platform/launch without socket activation the
// functions simply return (nil, nil).
package sdsocket

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// listenStart is the first inherited fd number per the sd_listen_fds
// protocol (SD_LISTEN_FDS_START).
const listenStart = 3

// ListenerByName returns the socket-activated net.Listener whose
// FileDescriptorName matches name, or (nil, nil) when no such fd was passed
// (not socket-activated, wrong PID, or no matching name). The caller owns
// the returned listener.
func ListenerByName(name string) (net.Listener, error) {
	return listenerByName(name, os.Getenv, os.Getpid, fdListener)
}

// listenerByName is the env-injectable core so the fd-resolution logic can
// be unit-tested without real inherited fds.
func listenerByName(
	name string,
	getenv func(string) string,
	getpid func() int,
	open func(fd int, name string) (net.Listener, error),
) (net.Listener, error) {
	pidStr := getenv("LISTEN_PID")
	if pidStr == "" {
		return nil, nil // not socket-activated
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid != getpid() {
		// The fds were meant for a different process (e.g. inherited
		// through an intermediary); ignore them.
		return nil, nil
	}
	n, err := strconv.Atoi(getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil, nil
	}
	var names []string
	if raw := getenv("LISTEN_FDNAMES"); raw != "" {
		names = strings.Split(raw, ":")
	}
	for i := 0; i < n; i++ {
		fdName := ""
		if i < len(names) {
			fdName = names[i]
		}
		if fdName != name {
			continue
		}
		return open(listenStart+i, name)
	}
	return nil, nil
}

// fdListener turns an inherited fd into a net.Listener. net.FileListener
// dups the fd, so we close our os.File copy afterwards: that removes the
// original fd from this process so it is not leaked into child processes
// (e.g. the ollama subprocess) — the listener keeps working on its dup.
func fdListener(fd int, name string) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, fmt.Errorf("sdsocket: invalid inherited fd %d", fd)
	}
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("sdsocket: fd %d (%q) is not a listening socket: %w", fd, name, err)
	}
	return ln, nil
}
