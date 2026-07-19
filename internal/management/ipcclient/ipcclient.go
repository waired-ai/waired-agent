// Package ipcclient dials the Local Management API's write endpoint — the
// unix-domain socket (Linux/macOS) or named pipe (Windows) that carries
// mutating requests (waired#838).
//
// It is shared by the `waired` CLI and the tray so endpoint resolution and
// the operator-facing error wording live in exactly one place, and so both
// agree with the daemon on which endpoint to use.
//
// Reads (status / identity / liveness) deliberately stay on the loopback
// TCP port, which the #836 Host/Origin/Content-Type guard already protects;
// only writes move here, because a browser or network peer cannot open a
// unix socket or a named pipe at all.
package ipcclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"
)

// BaseURL is the dummy authority write requests are addressed to. The
// transport's DialContext ignores host:port entirely and dials the local
// endpoint, but net/http still requires a syntactically valid absolute URL.
// The daemon serves the socket without browserGuard, so this Host is never
// validated.
const BaseURL = "http://waired-mgmt"

// Endpoint returns the local management write endpoint this client will
// dial. See the per-OS resolveEndpoint for the resolution order.
func Endpoint() string { return resolveEndpoint() }

// NewHTTPClient returns an http.Client whose transport dials the resolved
// local management endpoint instead of a TCP address. Keep-alives are
// disabled to match the short-lived, no-pool contract the tray and CLI
// already use for management calls.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return NewHTTPClientAt("", timeout)
}

// NewHTTPClientAt is NewHTTPClient pinned to a specific endpoint. An empty
// endpoint resolves per-OS at dial time. Used by tests and by the
// --mgmt-socket override.
func NewHTTPClientAt(endpoint string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				ep := endpoint
				if ep == "" {
					ep = resolveEndpoint()
				}
				return dial(ctx, ep)
			},
		},
	}
}

// WrapDialError turns a raw transport failure into wording an operator can
// act on. A missing endpoint almost always means the daemon is not running;
// a permission error means this user cannot reach the socket (which should
// not happen — the daemon opens it to all local users — and therefore
// points at a broken install rather than a policy decision).
func WrapDialError(err error) error {
	if err == nil {
		return nil
	}
	ep := Endpoint()
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied connecting to the waired management socket (%s); the socket should be world-connectable, so this suggests a broken install: %w", ep, err)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("waired management socket (%s) not found; is waired-agent running?: %w", ep, err)
	}
	return fmt.Errorf("waired management socket (%s): %w", ep, err)
}
