package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fastRetryDelay(t *testing.T) {
	t.Helper()
	old := headlessRetryBaseDelay
	headlessRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { headlessRetryBaseDelay = old })
}

// Transient failures (5xx) retry and succeed once the server recovers —
// the #352 shape: one racy 500 must not strand the login session.
func TestRetryHeadlessCompletion_TransientThenSuccess(t *testing.T) {
	fastRetryDelay(t)
	calls := 0
	err := retryHeadlessCompletion(context.Background(), "test", func(context.Context) error {
		calls++
		if calls < 3 {
			return &completionError{status: 500, msg: "internal_error"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after transient failures, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

// 409 means the session already left "waiting" — usually our own earlier
// attempt whose response was lost. Defer to the poll loop: no error, no
// further attempts.
func TestRetryHeadlessCompletion_ConflictDefersToPoll(t *testing.T) {
	fastRetryDelay(t)
	calls := 0
	err := retryHeadlessCompletion(context.Background(), "test", func(context.Context) error {
		calls++
		return &completionError{status: http.StatusConflict, msg: "session already authorized"}
	})
	if err != nil {
		t.Fatalf("409 must not be an error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("409 must stop retries, got %d attempts", calls)
	}
}

// Non-409 4xx is a configuration problem (e.g. 404 bypass_idp_disabled):
// permanent, immediate.
func TestRetryHeadlessCompletion_4xxIsPermanent(t *testing.T) {
	fastRetryDelay(t)
	calls := 0
	err := retryHeadlessCompletion(context.Background(), "test", func(context.Context) error {
		calls++
		return &completionError{status: 404, msg: "bypass_idp_disabled"}
	})
	if err == nil {
		t.Fatal("expected permanent error")
	}
	if calls != 1 {
		t.Fatalf("4xx must not retry, got %d attempts", calls)
	}
}

// Plain transport errors retry until the attempt budget runs out.
func TestRetryHeadlessCompletion_ExhaustsBudget(t *testing.T) {
	fastRetryDelay(t)
	calls := 0
	err := retryHeadlessCompletion(context.Background(), "test", func(context.Context) error {
		calls++
		return fmt.Errorf("dial tcp: connection refused")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != headlessCompletionAttempts {
		t.Fatalf("expected %d attempts, got %d", headlessCompletionAttempts, calls)
	}
	if !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("error should mention giving up, got %v", err)
	}
}

func TestRetryHeadlessCompletion_CtxCancelStopsBackoff(t *testing.T) {
	old := headlessRetryBaseDelay
	headlessRetryBaseDelay = time.Hour // force the cancel path, not the timer
	t.Cleanup(func() { headlessRetryBaseDelay = old })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := retryHeadlessCompletion(ctx, "test", func(context.Context) error {
		return fmt.Errorf("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// bypassCompleteLogin must surface the HTTP status as a *completionError
// so the retry loop can classify it.
func TestBypassCompleteLoginTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"internal_error"}}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := bypassCompleteLogin(context.Background(), srv.Client(), srv.URL, "ls_x", "a@b")
	var ce *completionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *completionError, got %T: %v", err, err)
	}
	if ce.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", ce.status)
	}
}

func TestOIDCGrantCompleteLoginTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"session_not_pending"}}`, http.StatusConflict)
	}))
	defer srv.Close()

	err := oidcGrantCompleteLogin(context.Background(), srv.Client(), srv.URL, "ls_x", "tok")
	var ce *completionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *completionError, got %T: %v", err, err)
	}
	if ce.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", ce.status)
	}
}
