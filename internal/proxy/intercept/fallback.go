package intercept

import (
	"bytes"
	"io"
	"maps"
	"net/http"
)

// readCappedBody reads up to max bytes of r.Body for a possible fallback
// replay. On success (whole body within max) it returns (body, true) with
// r.Body drained and closed — the caller supplies fresh readers to the
// local dispatch and any retry. When the body is unreadable or exceeds the
// cap it restores r.Body to the full stream (already-read prefix + the
// untouched remainder) and returns (nil, false), signalling the caller to
// serve transparently without buffering (bounding memory).
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

// fallbackRecorder wraps the client ResponseWriter for auto-mode's
// post-dispatch fallback. It defers everything the local handler writes
// until it can tell whether the response is committed:
//
//   - A status < 400 (via WriteHeader, or the implicit 200 on the first
//     Write/Flush) COMMITS: the staged headers + status are written to the
//     client and all further writes stream straight through. Streaming
//     (SSE) and normal 2xx responses are unaffected past the first byte.
//   - A status >= 400 with no committed body leaves the response
//     uncommitted; the caller may discard it and retry via passthrough
//     (eligibleForFallback).
//
// A conversion/validation error (e.g. the #578 system_blocks 400) is
// produced before the engine is called — always before any 2xx — so it is
// always recoverable; a failure after a 200 has been sent is not
// (fail-fast). Headers the handler sets are staged on a private map and
// only copied onto the real writer on commit, so an uncommitted error
// never pollutes the writer the fallback passthrough then uses.
type fallbackRecorder struct {
	w           http.ResponseWriter
	staged      http.Header
	status      int
	wroteHeader bool
	committed   bool
	buf         bytes.Buffer // body buffered while uncommitted (small error bodies only)
}

func newFallbackRecorder(w http.ResponseWriter) *fallbackRecorder {
	return &fallbackRecorder{w: w, staged: make(http.Header)}
}

func (rec *fallbackRecorder) Header() http.Header {
	if rec.committed {
		return rec.w.Header()
	}
	return rec.staged
}

func (rec *fallbackRecorder) WriteHeader(code int) {
	if rec.committed {
		rec.w.WriteHeader(code)
		return
	}
	if rec.wroteHeader {
		return // superfluous WriteHeader, mirror net/http
	}
	rec.wroteHeader = true
	rec.status = code
	if code < 400 {
		rec.commit()
	}
}

func (rec *fallbackRecorder) Write(p []byte) (int, error) {
	if rec.committed {
		return rec.w.Write(p)
	}
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK) // implicit 200 → commits
	}
	if rec.committed {
		return rec.w.Write(p)
	}
	// status >= 400 and uncommitted: buffer the (small) error body.
	return rec.buf.Write(p)
}

// Flush implements http.Flusher so the gateway's streaming path works.
func (rec *fallbackRecorder) Flush() {
	if !rec.committed {
		if !rec.wroteHeader {
			rec.WriteHeader(http.StatusOK) // a flush implies committing to a body
		}
		if !rec.committed {
			// An error status was set and flushed: keep holding it for a
			// possible fallback (the gateway never flushes error bodies).
			return
		}
	}
	if f, ok := rec.w.(http.Flusher); ok {
		f.Flush()
	}
}

// commit copies staged headers onto the real writer, sends the status, and
// flushes any body buffered before the commit (only reachable via the
// implicit-200 Write path).
func (rec *fallbackRecorder) commit() {
	maps.Copy(rec.w.Header(), rec.staged)
	rec.committed = true
	rec.w.WriteHeader(rec.status)
	if rec.buf.Len() > 0 {
		_, _ = rec.w.Write(rec.buf.Bytes())
		rec.buf.Reset()
	}
}

// eligibleForFallback reports whether the handler finished having produced
// only an uncommitted error (>=400) — safe to discard and retry upstream.
func (rec *fallbackRecorder) eligibleForFallback() bool {
	return !rec.committed && rec.wroteHeader && rec.status >= 400
}

// flushBuffered emits a non-fallback, uncommitted response to the client:
// a handler that wrote nothing (implicit 200) or, defensively, a buffered
// error the caller chose not to retry.
func (rec *fallbackRecorder) flushBuffered() {
	if rec.committed {
		return
	}
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
		return
	}
	rec.commit()
}
