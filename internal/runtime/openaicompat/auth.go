package openaicompat

import "net/http"

// bearerRT is the http.RoundTripper Adapter hands to the gateway via
// the runtime.Transporter optional interface. It injects
// "Authorization: Bearer <token>" on every outbound request when the
// token captured at NewAdapter time was non-empty.
//
// The base RoundTripper defaults to http.DefaultTransport. Tests
// inject their own by setting bearerRT.base directly.
type bearerRT struct {
	token string
	base  http.RoundTripper
}

// RoundTrip clones the request before mutating headers so a caller's
// stored *http.Request is not aliased. The base transport handles the
// actual network I/O.
func (r *bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
