package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Retry driver for headless login-session completion (--bypass-mode and
// --google-sa-login). Both flows complete the session programmatically,
// so a completion failure used to leave the session in "waiting" while
// RunInit's poll loop silently burned its whole 10-minute budget — the
// 10-minute enroll stall of #352 (one racy 500 from the CP, #353, cost a
// testnet run ~11 minutes). Transient failures now retry with backoff,
// and permanent ones surface fast so the caller can abort the init
// instead of out-waiting a session that will never authorize.

// completionError carries the HTTP status of a failed completion
// attempt so retryHeadlessCompletion can classify it. Transport-level
// failures (no response) stay plain errors and are treated as
// transient.
type completionError struct {
	status int
	msg    string
}

func (e *completionError) Error() string {
	return fmt.Sprintf("status %d: %s", e.status, e.msg)
}

// headlessCompletionAttempts bounds the retry loop: with the 1s base
// delay doubling each time (1,2,4,8,16s), 6 attempts ≈ 31s worst case
// before the failure is declared permanent.
const headlessCompletionAttempts = 6

// headlessRetryBaseDelay is a var so tests can shrink the backoff.
var headlessRetryBaseDelay = time.Second

// retryHeadlessCompletion runs attempt until it succeeds, is classified
// permanent, or the budget runs out. Classification:
//   - nil → done
//   - 409 → the session already left "waiting" (almost always our own
//     earlier attempt whose response was lost) → defer to the poll
//     loop, not an error
//   - other 4xx → configuration problem (e.g. 404 bypass_idp_disabled)
//     → permanent, no retry
//   - 5xx / transport errors → transient → retry with backoff
func retryHeadlessCompletion(ctx context.Context, label string, attempt func(context.Context) error) error {
	delay := headlessRetryBaseDelay
	for i := 1; ; i++ {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		var ce *completionError
		if errors.As(err, &ce) {
			switch {
			case ce.status == http.StatusConflict:
				fmt.Fprintf(os.Stderr, "%s: session no longer pending (%v); deferring to the poll loop\n", label, err)
				return nil
			case ce.status >= 400 && ce.status < 500:
				return fmt.Errorf("%s: permanent: %w", label, err)
			}
		}
		if i >= headlessCompletionAttempts {
			return fmt.Errorf("%s: giving up after %d attempts: %w", label, i, err)
		}
		fmt.Fprintf(os.Stderr, "%s: completion attempt %d failed (%v); retrying in %s\n", label, i, err, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}
