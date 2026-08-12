package ipcclient

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewReadClient returns an http.Client for management reads (waired#836).
// It dials the local IPC endpoint first and retries over tcpBase when the
// endpoint cannot be opened.
//
// That pairs with the daemon's own fail-open: while the socket is bound the
// daemon serves only the compatibility routes over TCP, so a read has to go
// to the socket; while it is not bound the daemon serves everything over
// TCP, and this client's fallback is what finds it there. It also keeps a
// mock daemon that has no socket at all (scripts/dev/mock-mgmt) working
// without changes.
//
// An empty tcpBase disables the fallback. Requests are addressed to
// BaseURL, exactly like writes; the fallback rewrites the authority.
func NewReadClient(tcpBase string, timeout time.Duration) *http.Client {
	return NewReadClientAt("", tcpBase, timeout)
}

// NewReadClientAt is NewReadClient pinned to a specific IPC endpoint. An
// empty endpoint resolves per-OS at dial time. Used by tests and by the
// --mgmt-socket override.
func NewReadClientAt(endpoint, tcpBase string, timeout time.Duration) *http.Client {
	rt := &readRoundTripper{
		socket: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				ep := endpoint
				if ep == "" {
					ep = resolveEndpoint()
				}
				return dial(ctx, ep)
			},
		},
		tcp: &http.Transport{DisableKeepAlives: true},
	}
	if u, err := url.Parse(normalizeBase(tcpBase)); err == nil && u.Host != "" {
		rt.tcpBase = u
	}
	return &http.Client{Timeout: timeout, Transport: rt}
}

// readRoundTripper sends over the IPC endpoint and falls back to loopback
// TCP when that endpoint is not there.
type readRoundTripper struct {
	socket  *http.Transport
	tcp     *http.Transport
	tcpBase *url.URL // nil disables the fallback
}

func (rt *readRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.socket.RoundTrip(req)
	if err == nil || rt.tcpBase == nil || !endpointUnavailable(err) {
		return resp, err
	}
	// Only a bodyless request is safe to replay: the reads this client
	// serves are all GET, and a RoundTripper may not consume a body twice.
	if req.Body != nil {
		return nil, err
	}
	fallback := req.Clone(req.Context())
	fallback.URL.Scheme = rt.tcpBase.Scheme
	fallback.URL.Host = rt.tcpBase.Host
	// Drop the dummy IPC authority so the TCP request carries a loopback
	// Host — the daemon's browserGuard allow-lists it (waired#836).
	fallback.Host = ""
	return rt.tcp.RoundTrip(fallback)
}

// endpointUnavailable reports whether err means "the local endpoint is not
// there", as opposed to a transport or protocol failure once connected.
// Unix returns a dial *net.OpError (ENOENT / ECONNREFUSED / EACCES);
// winio's DialPipe returns a *os.PathError carrying ERROR_FILE_NOT_FOUND.
func endpointUnavailable(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// SameAuthority reports whether two management base URLs address the same
// host:port. It tolerates a missing scheme and a trailing slash, both of
// which appear among the CLI's own defaults (`--mgmt` takes
// "http://127.0.0.1:9476" for most subcommands and the bare
// "127.0.0.1:9476" for peers / worker / claude route).
//
// Callers use it to decide whether a read may go over the socket at all: a
// base URL the operator changed names some other daemon (a mock on another
// port, a remote debug tunnel), and must not be quietly redirected to the
// local socket.
func SameAuthority(a, b string) bool {
	authA := authorityOf(a)
	return authA != "" && authA == authorityOf(b)
}

func authorityOf(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	u, err := url.Parse(normalizeBase(s))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// normalizeBase gives a scheme to a bare host:port so url.Parse reads it as
// an authority rather than a path.
func normalizeBase(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" || strings.Contains(s, "://") {
		return s
	}
	return "http://" + s
}
