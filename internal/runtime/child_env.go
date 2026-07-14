package runtime

import "strings"

// ChildBaseEnv returns parent extended with the two launch-environment
// guarantees every spawned engine child needs — the axis the #22 macOS
// "$HOME is not defined" crash exposed, unified here so the ollama, vLLM,
// and codeui spawn paths assemble their env one way instead of each
// hand-rolling it:
//
//   - HOME is present and non-empty. Set to homeFallback when the parent
//     has no HOME (macOS system LaunchDaemons start HOME-less; systemd
//     derives one from User= and the Windows SCM provides %USERPROFILE%).
//     An inherited non-empty HOME is preserved (a guard, not an override),
//     and HOME is never fabricated when homeFallback == "" (nothing
//     writable to point at).
//   - When extraPathDir != "", it is prepended to PATH so a launcher that
//     hands the child a stripped PATH (launchd gives a system daemon only
//     /usr/bin:/bin:/usr/sbin:/sbin) can still find sidecars next to the
//     engine binary.
//
// goos is threaded in rather than read from runtime.GOOS so the policy is
// table-testable across linux/windows/darwin and any future per-OS
// divergence has a single home; today it only selects case-insensitive
// PATH matching on Windows (os.Environ may yield "Path="). pathSep is
// string(os.PathListSeparator), passed so tests don't depend on the host.
func ChildBaseEnv(goos string, parent []string, homeFallback, extraPathDir, pathSep string) []string {
	isPathKey := func(k string) bool {
		if goos == "windows" {
			return strings.EqualFold(k, "PATH")
		}
		return k == "PATH"
	}

	var curHome, curPath string
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "HOME":
			curHome = v
		case isPathKey(k):
			curPath = v
		}
	}

	setHome := curHome == "" && homeFallback != ""
	setPath := extraPathDir != ""

	out := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			// Drop the keys we are about to (re)write so our value wins
			// regardless of getenv's first-vs-last duplicate resolution.
			if setHome && k == "HOME" {
				continue
			}
			if setPath && isPathKey(k) {
				continue
			}
		}
		out = append(out, kv)
	}
	if setHome {
		out = append(out, "HOME="+homeFallback)
	}
	if setPath {
		if curPath != "" {
			out = append(out, "PATH="+extraPathDir+pathSep+curPath)
		} else {
			out = append(out, "PATH="+extraPathDir)
		}
	}
	return out
}
