package claudemanaged

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// utf8BOM is the byte-order mark Windows editors and PowerShell put at the head
// of a UTF-8 file: `Set-Content -Encoding utf8` (5.1), `>` redirection, and
// Notepad's "UTF-8 with BOM" all produce one. encoding/json rejects it.
//
// Claude Code reads such a file without complaint, so refusing one made waired
// the odd one out on the platform where it is easiest to acquire — and made it
// fail in the worst available way: routing kept working, because Claude Code
// still read the base URL, while every waired surface reported routing as
// absent and the SessionStart hook stopped writing the /model picker entries.
// The one signal that would have contradicted "waired is not routing" was the
// signal that broke (waired-agent#1067).
//
// internal/integration/openclaw/openclawjson.go made the same allowance for the
// same reason; the Claude side did not get it until now.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// errSettingsUnparseable marks a managed-settings file that exists and is not
// JSON this package can read. It is a THIRD state, distinct from "absent" and
// from "present with the key unset", and the readers below keep it distinct.
//
// Collapsing it was the other half of waired-agent#1067: ViewAt returned
// (present=true, baseURL="") for a file it could not parse, which is exactly
// what a file with no ANTHROPIC_BASE_URL returns — so `waired claude status`
// said "(not set)" about a file that in fact set it. Stripping the BOM removes
// today's trigger for that; keeping the states apart is what makes the next one
// legible instead of a silent lie.
var errSettingsUnparseable = errors.New("claudemanaged: settings file is not readable JSON")

// readSettingsObject parses the managed-settings document at path.
//
// present says the file exists — even when it cannot be parsed, because "there
// is a file here and something is wrong with it" is a different thing to report
// than "there is no file". A leading UTF-8 BOM is tolerated. A blank file
// parses as an empty object: an operator who truncated the file has not written
// anything malformed.
func readSettingsObject(path string) (obj map[string]any, present bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("claudemanaged: read %s: %w", path, err)
	}
	b = bytes.TrimPrefix(b, utf8BOM)
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, true, nil
	}
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, true, fmt.Errorf("%w: %s", errSettingsUnparseable, path)
	}
	return obj, true, nil
}

// envString reads one env value out of a parsed settings object.
func envString(obj map[string]any, key string) string {
	env, ok := obj["env"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := env[key].(string)
	return v
}
