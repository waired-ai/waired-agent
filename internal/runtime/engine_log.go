package runtime

import (
	"fmt"
	"strings"
	"time"
)

// engineLogSpawnBanner opens the line the vLLM adapter writes to
// <LogDir>/engine.log immediately before each spawn. It exists because
// that file holds several spawns: bootstrapVLLM makes up to three
// start-up attempts and every one of them appends
// (waired-ai/waired-agent#878). Without a delimiter, three attempts read
// as one confusing run, and a reader cannot tell which line belongs to
// which attempt.
//
// The exact text is a format contract with LastEngineLogSpawn below and
// with everything that reads back a spawn's own output — the KV-capacity
// read-back and the start-up diagnosis in cmd/waired-agent. Changing it
// without changing those is how a reader silently starts scoping to
// nothing.
const engineLogSpawnBanner = "===== waired: vllm spawn"

// engineLogSpawnBannerLine renders one banner. Leading newline is the
// caller's job (see openEngineLog): on a fresh file it would leave a
// blank first line, and on an appended-to file it is what keeps the
// banner off the end of a previous, possibly partial, line.
func engineLogSpawnBannerLine(now time.Time) string {
	return fmt.Sprintf("%s %s =====\n", engineLogSpawnBanner, now.UTC().Format(time.RFC3339))
}

// LastEngineLogSpawn returns the section of an engine.log written by the
// most recent spawn: everything from the last banner onwards.
//
// A log with no banner is returned whole. That is the right answer for
// both cases where it happens: a file written by an agent that predates
// #878 holds exactly one spawn (it truncated per spawn), and an ollama
// engine.log — which rotates rather than appends — also holds exactly
// one. In both, the whole file IS the last spawn.
//
// Readers want this rather than the whole file whenever they are asking
// what the RUNNING engine did. A "GPU KV cache size" line from a spawn
// that has since been replaced describes a configuration that is no
// longer loaded, and reporting it as the current engine's measured
// capacity would present a stale number as a measurement.
func LastEngineLogSpawn(log string) string {
	if i := strings.LastIndex(log, engineLogSpawnBanner); i >= 0 {
		return log[i:]
	}
	return log
}
