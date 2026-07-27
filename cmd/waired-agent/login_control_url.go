package main

import (
	"log/slog"

	"github.com/waired-ai/waired-agent/internal/controlurl"
)

// resolveDaemonControlURL picks the Control Plane the app's "Log in…"
// enrolls against when the login request itself carries no control_url,
// applying the same three tiers as `waired init`: an explicit --control /
// $WAIRED_CONTROL_URL, then the installer-recorded agent.env, then the
// baked production Control Plane.
//
// Before #174 the daemon had only the first tier, and nothing fills it on
// an ordinary install: the launchd plist and the Windows SCM cannot feed
// a service an env file at all, and Linux's systemd EnvironmentFile only
// carries a URL when install.sh was given --control/--dev. So a stock,
// not-yet-enrolled device failed sign-in from the app on every OS with
// "login: no control URL". The daemon half of #42, whose fix (#173)
// covered `waired init` only.
//
// Unlike the CLI this never fails the process. A malformed value is
// logged and skipped in favour of the next tier: the daemon does far more
// than login — the overlay, inference and the management API all have to
// come up — so a typo in agent.env must not keep it from booting. The
// returned value is always normalized, or "" if nothing survives (which
// leaves loginController.Start reporting its own "no control URL", the
// pre-#174 behaviour, rather than silently enrolling somewhere else).
func resolveDaemonControlURL(explicit, platformDefault string, logger *slog.Logger) string {
	for _, tier := range []struct {
		source string
		raw    string
	}{
		{"--control / $WAIRED_CONTROL_URL", explicit},
		{"agent.env", platformDefault},
		{"built-in default", controlurl.Default},
	} {
		if tier.raw == "" {
			continue
		}
		norm, err := controlurl.Normalize(tier.raw)
		if err != nil {
			logger.Error("login: ignoring malformed control URL; falling back to the next source",
				"source", tier.source, "value", tier.raw, "err", err)
			continue
		}
		return norm
	}
	return ""
}
