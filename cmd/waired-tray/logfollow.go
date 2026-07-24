package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/management"
)

// flagPassed reports whether the named flag was explicitly set on the
// command line (as opposed to left at its default). fs.Visit only visits
// flags that were actually set.
func flagPassed(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// followDaemonLogLevel polls the daemon's GET /waired/v1/log/level and
// mirrors it onto levelVar, so `waired config log-level <level>` flips the
// tray's verbosity in lockstep with the service — without restarting the
// tray. Best-effort: an unreachable daemon or an older build (non-200)
// leaves the current level in place until the next tick. Returns when ctx
// is cancelled.
func followDaemonLogLevel(ctx context.Context, mgmtURL string, levelVar *slog.LevelVar, every time.Duration) {
	if every <= 0 {
		every = 5 * time.Second
	}
	base := strings.TrimRight(mgmtURL, "/")
	hc := &http.Client{Timeout: 4 * time.Second}

	apply := func() {
		lvl, ok := fetchDaemonLogLevel(ctx, hc, base)
		if !ok || levelVar.Level() == lvl {
			return
		}
		slog.Info("tray log level now follows daemon", "level", agentconfig.LogLevelName(lvl))
		levelVar.Set(lvl)
	}

	apply()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			apply()
		}
	}
}

// fetchDaemonLogLevel does one GET /waired/v1/log/level and parses it.
// The bool is false on any error (unreachable, non-200, decode, unknown
// level) so callers keep the current level rather than react to noise.
func fetchDaemonLogLevel(ctx context.Context, hc *http.Client, base string) (slog.Level, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/waired/v1/log/level", nil)
	if err != nil {
		return 0, false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var body management.LogLevelResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, false
	}
	lvl, err := agentconfig.ParseLogLevel(body.Level)
	if err != nil {
		return 0, false
	}
	return lvl, true
}
