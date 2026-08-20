package controlclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// DefaultEnrollRetryBackoff is how long RunInit waits between attempts at
// the two control-plane calls that stand between a host and an enrolled
// device. Four attempts, ~19 seconds of waiting.
//
// Sized against how long a control-plane instance is gone for. A Cloud Run
// instance that dies mid-request — OOM-killed, or drained on a revision
// roll — takes every in-flight request with it and reports each as
// `503 Service Unavailable`; a replacement is serving again within tens of
// seconds. waired#1237 has the case that prompted this: two runs 31 seconds
// apart against the same control plane, one enrolled in 6 seconds and the
// other failed outright.
var DefaultEnrollRetryBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 12 * time.Second}

// maxRetryAfter caps how long a Retry-After header can hold us. The header
// is advice from a server that is already unhealthy; honouring an
// unbounded value would let it hang the installer.
const maxRetryAfter = 30 * time.Second

// httpStatusError is a non-200 control-plane response. It carries the
// status so callers can tell a transient failure from a verdict, and
// formats exactly as the bare error it replaced ("status %d: %s") so
// existing messages, logs and docs are unchanged.
type httpStatusError struct {
	StatusCode int
	Body       []byte
	// RetryAfter is the parsed Retry-After header, or 0 when absent or
	// unparseable.
	RetryAfter time.Duration
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.StatusCode, e.Body)
}

// retryable reports whether err is worth a second attempt: a transport
// failure, or a status that says "not now" rather than "no".
//
// 500 is deliberately absent. A control plane that answers 500 handled the
// request and failed deterministically; repeating it just delays the same
// answer. 429/502/503/504 are the transient set, matching the one the e2e
// budget prober already classifies (internal/e2e/integration/budget.go).
func retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired caller context is the caller's decision, not
	// the server's — never retry through it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	// checkHTTPResponse's other verdict: --control points at something
	// that is not a control-plane API. Retrying cannot fix a wrong URL.
	var ne *notAnAPIEndpointError
	if errors.As(err, &ne) {
		return false
	}
	// Everything left is a transport failure — a connection reset, an EOF
	// part-way through the response, a DNS blip. An instance killed
	// mid-response produces these as readily as it produces a 503.
	return true
}

// retryDelay is how long to wait before attempt n+1: the server's
// Retry-After when it gave a usable one, else the configured backoff.
func retryDelay(err error, fallback time.Duration) time.Duration {
	var se *httpStatusError
	if errors.As(err, &se) && se.RetryAfter > 0 {
		if se.RetryAfter > maxRetryAfter {
			return maxRetryAfter
		}
		return se.RetryAfter
	}
	return fallback
}

// parseRetryAfter reads the delay-seconds form of Retry-After. The
// HTTP-date form is not parsed: it needs a trusted clock on both ends, and
// no control-plane response uses it.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// withRetry runs do until it succeeds, returns a non-retryable error, or
// runs out of backoff steps. The last error is returned as-is, so a caller
// that gives up still reports what the control plane actually said.
func withRetry[T any](ctx context.Context, backoff []time.Duration, sleep func(context.Context, time.Duration) error, do func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		got, err := do()
		if err == nil {
			return got, nil
		}
		if attempt >= len(backoff) || !retryable(err) {
			return zero, err
		}
		if serr := sleep(ctx, retryDelay(err, backoff[attempt])); serr != nil {
			return zero, err
		}
	}
}

// sleepCtx waits for d, or returns the context's error if it ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
