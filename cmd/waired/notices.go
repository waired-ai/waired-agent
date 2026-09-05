package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/management"
	notices "github.com/waired-ai/waired-agent/internal/notice"
)

// fetchNotices reads what the daemon is currently publishing for a
// person to read (waired-agent#1205).
//
// A daemon that predates the route answers 404, which is reported here
// as "nothing to say" rather than as an error: on both surfaces that
// call this, an older daemon and a healthy one have the same thing to
// show, which is nothing. Any other failure is returned so the caller
// can decide — `waired status` stays quiet about it, since the block is
// best-effort like the inference summary above it.
//
// The read goes through httpGet, which routes over the local IPC socket
// with a TCP fallback. The notice route is deliberately socket-only, so
// building a client here instead would 403 against a daemon with its
// socket up.
func fetchNotices(mgmtURL string) ([]notices.Notice, error) {
	body, err := httpGet(mgmtURL + "/waired/v1/notices")
	if err != nil {
		var se *mgmtStatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var resp management.NoticesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return notices.Clamp(resp.Notices), nil
}

// printNotices renders the daemon's notices as a block, or nothing at
// all when there are none.
//
// Nothing rather than an empty heading: on a healthy computer there is
// usually nothing to say, and a standing "Notices:" with no rows under
// it would be a section people learn to skip. Silent on a read failure
// for the same reason the inference summary above it is — the daemon
// being unreachable is already reported, once, further up.
func printNotices(mgmtURL string) {
	ns, err := fetchNotices(mgmtURL)
	if err != nil || len(ns) == 0 {
		return
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Notices:")
	for _, n := range ns {
		if n.Title == "" {
			continue
		}
		fmt.Fprintf(stdout, "  %s %s\n", noticeMark(n.Severity), n.Title)
		if n.Text != "" {
			fmt.Fprintf(stdout, "    %s\n", n.Text)
		}
	}
}

// noticeMark is the marker a severity gets on a terminal. It goes
// through emo() so a console that cannot draw the glyph gets the ASCII
// fallback instead of mojibake; both marks are already in the fold table
// (ascii.go). Info's is an arrow because every Info notice today is a
// step-up model suggestion, which `waired init` already marks that way —
// a record of today's producers, not a rule about severities.
func noticeMark(s notices.Severity) string {
	if s == notices.SeverityWarn {
		return emo("⚠", "!")
	}
	return emo("⬆", "^")
}
