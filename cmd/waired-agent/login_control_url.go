package main

import (
	"log/slog"

	"github.com/waired-ai/waired-agent/internal/controlurl"
)

// resolveDaemonControlURL picks the Control Plane the app's "Sign in…"
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
	url, _ := resolveDaemonControlURLWithSource(explicit, platformDefault, logger)
	return url
}

// resolveDaemonControlURLWithSource is resolveDaemonControlURL plus which
// tier answered, for the log line a sign-in writes.
func resolveDaemonControlURLWithSource(explicit, platformDefault string, logger *slog.Logger) (string, string) {
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
		return norm, tier.source
	}
	return "", ""
}

// explicitControlURL is the daemon's explicit tier: what --control or
// $WAIRED_CONTROL_URL names, minus a value that only came from agent.env.
//
// On a deb install the systemd unit loads agent.env with EnvironmentFile,
// so a URL in the file reaches the daemon as $WAIRED_CONTROL_URL too. Taken
// as explicit, that copy is fixed at service start and outranks the file
// the resolver reads again at every sign-in: an agent.env edited without a
// restart lost to its own old value (waired-agent#1377). So when --control
// was not passed and the environment's URL is the one agent.env held at
// boot, the value is treated as agent.env's and the explicit tier is left
// empty. A --control flag, or an environment URL that differs from the
// file, stays explicit.
//
// This is not limited to Linux. launchd and the Windows SCM give the
// service no env file, so the two can only match there when someone set
// both to the same URL by hand, and following the file is then harmless.
func explicitControlURL(flagValue string, flagSet bool, env, agentEnvAtBoot string) string {
	if flagSet {
		return flagValue
	}
	if env == "" || agentEnvAtBoot == "" {
		return env
	}
	e, errE := controlurl.Normalize(env)
	f, errF := controlurl.Normalize(agentEnvAtBoot)
	if errE == nil && errF == nil && e == f {
		return ""
	}
	return env
}

// newDaemonControlURLResolver returns the resolver a sign-in calls when
// its request names no control plane. explicit is the daemon's own
// --control / $WAIRED_CONTROL_URL, fixed at boot; platformDefault is read
// again on every call.
//
// Resolving once at boot was not enough (waired-agent#1343). The macOS
// and Linux installers start the service before they write agent.env
// (launchd RunAtLoad and the deb's postinst start it; the file is written
// after), so a daemon that has not restarted since install still held the
// production default, and the app's "Sign in…" — or any request that left
// the control plane to the daemon — went to production.
func newDaemonControlURLResolver(explicit string, platformDefault func() string, logger *slog.Logger) func() (string, string) {
	return func() (string, string) {
		return resolveDaemonControlURLWithSource(explicit, platformDefault(), logger)
	}
}
