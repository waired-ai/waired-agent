package detect

import (
	"errors"
	"io/fs"
	"os"

	"github.com/waired-ai/waired-agent/internal/integration/shellalias"
)

// ShellAlias inspects ~/.bashrc, ~/.zshrc, and ~/.config/fish/config.fish
// for the waired-claude alias sentinel block, reporting per file.
//
// expectedCommand is what the alias should resolve to (typically
// `<absolute-waired-binary> claude`). Mismatch ⇒ Stale=true.
//
// Files that do not exist are omitted from the result entirely (no
// noise in the tray). Files that exist but lack the sentinel return
// Configured=false. Files that have the sentinel but cannot be
// parsed return Configured=true / Stale=true / Note="unparseable".
func ShellAlias(homeDir, expectedCommand string) []Result {
	var out []Result
	for _, c := range shellalias.RCCandidates(homeDir) {
		body, err := os.ReadFile(c.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		r := Result{Path: c.Path}
		if err != nil {
			r.Note = "read-error: " + err.Error()
			out = append(out, r)
			continue
		}
		start, end, ok := shellalias.FindBlock(body)
		if !ok {
			r.Configured = false
			out = append(out, r)
			continue
		}
		got := shellalias.ExtractCommand(body[start:end], c.Fish)
		r.Configured = true
		r.CurrentValue = got
		switch {
		case got == "":
			r.Stale = true
			r.Note = "unparseable"
		case got != expectedCommand:
			r.Stale = true
		}
		out = append(out, r)
	}
	return out
}
