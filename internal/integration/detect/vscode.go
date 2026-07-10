package detect

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// vscodeFlavors enumerates the VSCode-family installs we look at.
// Order is significant only for stable JSON output.
var vscodeFlavors = []struct {
	Flavor string
	Subdir string // under ~/.config
}{
	{Flavor: "Code", Subdir: "Code/User/settings.json"},
	{Flavor: "Code-Insiders", Subdir: "Code - Insiders/User/settings.json"},
	{Flavor: "VSCodium", Subdir: "VSCodium/User/settings.json"},
}

// VSCodeWrapper inspects each known VSCode-family settings.json for
// the `claude.claudeProcessWrapper` key. Only existing files are
// reported. JSONC (`//` and `/* */` comments, plus trailing commas in
// objects/arrays) is tolerated by stripping comments and trailing
// commas before json.Unmarshal.
//
// On unparseable JSON the entry is reported with Configured=true,
// Stale=true, Note="unparseable" — the user almost certainly has
// *some* claude config they're going to be confused about, so we
// surface it rather than silently skip.
func VSCodeWrapper(homeDir, expectedCommand string) []Result {
	var out []Result
	for _, f := range vscodeFlavors {
		path := filepath.Join(homeDir, ".config", f.Subdir)
		body, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		r := Result{Path: path, Flavor: f.Flavor}
		if err != nil {
			r.Note = "read-error: " + err.Error()
			out = append(out, r)
			continue
		}
		stripped := stripJSONC(body)
		var settings map[string]any
		if err := json.Unmarshal(stripped, &settings); err != nil {
			r.Configured = true
			r.Stale = true
			r.Note = "unparseable: " + err.Error()
			out = append(out, r)
			continue
		}
		v, ok := settings["claude.claudeProcessWrapper"]
		if !ok {
			out = append(out, r)
			continue
		}
		s, _ := v.(string)
		r.Configured = true
		r.CurrentValue = s
		if s != expectedCommand {
			r.Stale = true
		}
		out = append(out, r)
	}
	return out
}

// stripJSONC removes // and /* */ comments and trailing commas from a
// JSONC blob, preserving comment-like sequences inside string
// literals. Not a full JSONC parser — sufficient for VSCode user
// settings, which are simple key/value maps.
func stripJSONC(in []byte) []byte {
	out := make([]byte, 0, len(in))
	const (
		stateNormal = iota
		stateString
		stateLineComment
		stateBlockComment
	)
	state := stateNormal
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch state {
		case stateNormal:
			if c == '"' {
				out = append(out, c)
				state = stateString
				continue
			}
			if c == '/' && i+1 < len(in) {
				switch in[i+1] {
				case '/':
					state = stateLineComment
					i++
					continue
				case '*':
					state = stateBlockComment
					i++
					continue
				}
			}
			out = append(out, c)
		case stateString:
			out = append(out, c)
			if c == '\\' && i+1 < len(in) {
				out = append(out, in[i+1])
				i++
				continue
			}
			if c == '"' {
				state = stateNormal
			}
		case stateLineComment:
			if c == '\n' {
				out = append(out, c)
				state = stateNormal
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(in) && in[i+1] == '/' {
				state = stateNormal
				i++
			}
		}
	}
	return removeTrailingCommas(out)
}

// removeTrailingCommas walks the comment-stripped bytes and drops any
// `,` that is followed (after whitespace) by `}` or `]`. String
// literals are tracked so commas inside strings are preserved.
func removeTrailingCommas(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(in) {
				out = append(out, in[i+1])
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			out = append(out, c)
			inString = true
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(in) && (in[j] == ' ' || in[j] == '\t' || in[j] == '\n' || in[j] == '\r') {
				j++
			}
			if j < len(in) && (in[j] == '}' || in[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
