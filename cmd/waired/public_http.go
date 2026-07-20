package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
)

// mgmtStatusError is a >=400 management-API response mapped to a typed
// error so callers can branch on the HTTP status (e.g. treat 404 as
// "route not present, degrade") and on the machine-readable error code
// without string-matching. The two Public Share route families emit two
// different JSON error shapes (see parseMgmtError); both land here.
type mgmtStatusError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *mgmtStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("status %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("status %d", e.StatusCode)
}

// parseMgmtError normalises a management error body into a
// *mgmtStatusError. It accepts BOTH shapes in use today:
//   - {"error_code":"…","message":"…"} — the errorBody() helper shared by
//     the consumer routes (public_use.go, inference handlers).
//   - {"error":"…"} — the plain shape the provider share routes write
//     (public_share.go).
//
// When neither parses (an http.Error plain-text body, or empty), the raw
// body is carried through as the message so the operator still sees it.
func parseMgmtError(status int, body []byte) *mgmtStatusError {
	e := &mgmtStatusError{StatusCode: status}

	var coded struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(body, &coded) == nil && (coded.ErrorCode != "" || coded.Message != "") {
		e.Code = coded.ErrorCode
		e.Message = coded.Message
		return e
	}

	var plain struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &plain) == nil && plain.Error != "" {
		e.Message = plain.Error
		return e
	}

	e.Message = strings.TrimSpace(string(body))
	return e
}

// isMgmtStatus reports whether err (or something it wraps) is a
// *mgmtStatusError carrying the given HTTP status code.
func isMgmtStatus(err error, code int) bool {
	var me *mgmtStatusError
	return errors.As(err, &me) && me.StatusCode == code
}

// mgmtErrorCode returns the machine-readable error_code of a
// *mgmtStatusError, or "" when err is not one (or carried no code).
func mgmtErrorCode(err error) string {
	var me *mgmtStatusError
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

// publicGetJSON issues a read against the Local Management API over the
// loopback TCP port (reads are allowed there; only writes are pinned to
// the IPC socket). A transport error is wrapped with the same
// daemon-unreachable wording the other read helpers use; a >=400 response
// becomes a *mgmtStatusError.
func publicGetJSON(mgmt, path string, out any) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(mgmt, "/") + path)
	if err != nil {
		return wrapDaemonDialError(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return parseMgmtError(resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// publicPostJSON issues a management WRITE. It routes through
// mgmtWriteRoute so the request travels over the local IPC socket in
// production (waired#838: the daemon's writeGuard 403s POSTs that arrive
// on the loopback TCP port). Transport errors are wrapped exactly as
// readMgmtResponse does — naming the socket or the TCP endpoint depending
// on which transport was used — and a >=400 response becomes a
// *mgmtStatusError.
func publicPostJSON(mgmt, path string, in, out any) error {
	var reqBody []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = b
	}

	target, client, viaSocket, err := mgmtWriteRoute(strings.TrimRight(mgmt, "/")+path, 10*time.Second)
	if err != nil {
		return err
	}
	resp, perr := client.Post(target, "application/json", bytes.NewReader(reqBody))
	if perr != nil {
		if viaSocket {
			return ipcclient.WrapDialError(perr)
		}
		return wrapDaemonDialError(perr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return parseMgmtError(resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}
