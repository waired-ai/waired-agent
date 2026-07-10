package detect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// JetBrains plugin settings vary by IDE / version / plugin layout.
// We accept two forms commonly observed for the Claude Code plugin:
//
//   <option name="claudeCommand" value="/path/to/waired claude"/>
//   <setting name="claudeCode.claudeCommand" value="/path/to/waired claude"/>
//
// and the properties form sometimes seen in `*.properties`:
//
//   claudeCode.claudeCommand=/path/to/waired claude
//
// Each detection site (file) becomes one Result entry. Multiple
// IDEs / versions ⇒ multiple entries. Files without the key are
// silently skipped.

var (
	xmlOptionRe = regexp.MustCompile(`<(?:option|setting)\s+name="(?:claudeCode\.)?claudeCommand"\s+value="([^"]*)"`)
	propsLineRe = regexp.MustCompile(`(?m)^\s*claudeCode\.claudeCommand\s*=\s*(.+?)\s*$`)
	jbVersionRe = regexp.MustCompile(`^[A-Za-z]+[A-Za-z0-9_+-]*\d`)
)

// JetBrainsWrapper inspects ~/.config/JetBrains/<IDE>-<ver>/options/
// for files containing claudeCode.claudeCommand (XML or properties
// form). Unmatching files are silently skipped; matching files
// produce one Result each.
func JetBrainsWrapper(homeDir, expectedCommand string) []Result {
	root := filepath.Join(homeDir, ".config", "JetBrains")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []Result{{Path: root, Note: "read-error: " + err.Error()}}
	}
	var out []Result
	for _, e := range entries {
		if !e.IsDir() || !jbVersionRe.MatchString(e.Name()) {
			continue
		}
		flavor := e.Name()
		optionsDir := filepath.Join(root, flavor, "options")
		files, err := os.ReadDir(optionsDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(optionsDir, f.Name())
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			value, ok := extractJetBrainsCommand(body)
			if !ok {
				continue
			}
			r := Result{
				Path:         path,
				Flavor:       flavor,
				Configured:   true,
				CurrentValue: value,
			}
			if value != expectedCommand {
				r.Stale = true
			}
			out = append(out, r)
		}
	}
	return out
}

func extractJetBrainsCommand(body []byte) (string, bool) {
	if m := xmlOptionRe.FindSubmatch(body); m != nil {
		return decodeXMLEntities(string(m[1])), true
	}
	if m := propsLineRe.FindSubmatch(body); m != nil {
		return strings.TrimSpace(string(m[1])), true
	}
	return "", false
}

// decodeXMLEntities reverses the small set of escapes typically seen
// in IntelliJ-written XML (`&quot;`, `&apos;`, `&amp;`, `&lt;`,
// `&gt;`). Anything else is left as-is.
func decodeXMLEntities(s string) string {
	r := strings.NewReplacer(
		"&quot;", `"`,
		"&apos;", "'",
		"&lt;", "<",
		"&gt;", ">",
		"&amp;", "&",
	)
	return r.Replace(s)
}
