package intercept

import (
	"bytes"
	"io"
	"net/http"
)

// readCappedBody reads up to max bytes of r.Body so the request can be
// inspected — the model id that says where the turn runs, and the auto-mode
// classifier's shape. On success (whole body within max) it returns
// (body, true) with r.Body drained and closed; the caller supplies a fresh
// reader to whichever leg it dispatches. When the body is unreadable or
// exceeds the cap it restores r.Body to the full stream (already-read prefix
// + the untouched remainder) and returns (nil, false), signalling the caller
// to serve it unexamined (bounding memory).
func readCappedBody(r *http.Request, max int64) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return []byte{}, true
	}
	orig := r.Body
	buf, err := io.ReadAll(io.LimitReader(orig, max+1))
	if err != nil || int64(len(buf)) > max {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), orig))
		return nil, false
	}
	_ = orig.Close()
	return buf, true
}
