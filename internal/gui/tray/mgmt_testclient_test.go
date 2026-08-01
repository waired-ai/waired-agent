package tray

import (
	"net/http"
	"strings"
)

// newTestClient returns a Client whose WRITE path targets baseURL (an
// httptest TCP server) instead of the local IPC socket.
//
// Since waired#838 production writes go over a unix socket / named pipe,
// which a plain httptest.NewServer cannot provide. These tests cover
// endpoint semantics — status codes, 404 sentinels, request payloads —
// which are transport-independent; the real socket transport is exercised
// end-to-end in mgmt_socket_unix_test.go.
func newTestClient(baseURL string) *Client {
	c := NewClient(baseURL)
	c.writeBase = strings.TrimRight(baseURL, "/")
	c.wc = &http.Client{Timeout: writeTimeout}
	// Keep the two budgets distinct, as production does: a test that
	// leans on the engine endpoints outlasting the cheap ones (#316)
	// must fail here if that separation is ever collapsed.
	c.wcEngine = &http.Client{Timeout: engineWriteTimeout}
	return c
}
