package claudemanaged

// Detecting a Claude Code an organisation already manages, before writing
// over its settings (waired-agent#1188).
//
// The file waired writes is machine-wide and root-owned, and waired has
// always merged into it rather than replacing it — every key it does not own
// rides through untouched. That was enough while the only question was "does
// some other key survive". It is not enough for ANTHROPIC_BASE_URL itself,
// and the asymmetry was visible in this package for a while: Remove strips
// that key only when it carries waired's own loopback prefix, so a disable
// cannot delete an operator's gateway — while Write overwrote it
// unconditionally.
//
// What the overwrite costs on a managed machine is documented rather than
// theoretical. A non-default ANTHROPIC_BASE_URL is listed under "Security
// considerations" as a way server-managed settings are BYPASSED: Claude Code
// skips the settings fetch for those sessions
// (https://code.claude.com/docs/en/server-managed-settings). So pointing the
// base URL at the local gateway on a Teams or Enterprise machine silently
// switches off centrally delivered policy for every user of that machine —
// which is a decision for whoever manages it, not for an installer.
//
// So: read first, and stop with an explanation when the file is somebody
// else's. This is a refusal, not a merge conflict — there is no version of
// "write anyway" that is safe here, because the thing being overwritten is
// the mechanism by which the owner would have said no.

import (
	"fmt"
	"sort"
	"strings"
)

// Organisation keys. Each one means the same thing: somebody who is not the
// person at this keyboard configured Claude Code on this machine.
const (
	forceLoginOrgKey     = "forceLoginOrgUUID"
	forceLoginMethodKey  = "forceLoginMethod"
	forceLoginGatewayKey = "forceLoginGatewayUrl"
	availableModelsKey   = "availableModels"
	modelPickerKey       = "modelPicker"
)

// OrgSignal is one reason waired will not write.
type OrgSignal struct {
	// Key is the setting that carries the signal, spelled as Claude Code
	// spells it, so the operator can find it in the file.
	Key string
	// Detail is what was found, when saying it helps. Empty when the
	// presence of the key is the whole story.
	Detail string
}

// String renders one signal for a message a person reads.
func (s OrgSignal) String() string {
	if s.Detail == "" {
		return s.Key
	}
	return s.Key + " = " + s.Detail
}

// OrgManaged reports the organisation signals in a managed-settings file, or
// nil when there are none. An absent or unreadable file yields nil: absent is
// not managed, and unreadable is refused by the write path for its own
// reasons (a file waired cannot parse is one it will not rewrite).
func OrgManaged() []OrgSignal { return OrgManagedAt(resolvePath()) }

// OrgManagedAt is OrgManaged against an explicit path.
func OrgManagedAt(path string) []OrgSignal {
	obj, present, err := readSettingsObject(path)
	if err != nil || !present {
		return nil
	}
	return orgSignalsIn(obj)
}

// orgSignalsIn is OrgManagedAt over an object already read. Write uses this
// one: re-reading the file to answer the question would leave a window
// between the check and the write in which the answer can change.
func orgSignalsIn(obj map[string]any) []OrgSignal {
	var out []OrgSignal
	// A login the organisation forces: the account, the method, or the
	// gateway it must go through. Any of the three means Claude Code is
	// being pointed somewhere by policy.
	for _, k := range []string{forceLoginOrgKey, forceLoginMethodKey, forceLoginGatewayKey} {
		if v, ok := obj[k]; ok && v != nil {
			out = append(out, OrgSignal{Key: k, Detail: shortValue(v)})
		}
	}
	// An allowlist of models, or a picker lineup. Both say somebody curated
	// what this machine's users may choose — and waired's own rows would
	// either be filtered out by the first or replace the second whole.
	for _, k := range []string{availableModelsKey, modelPickerKey} {
		if v, ok := obj[k]; ok && v != nil {
			out = append(out, OrgSignal{Key: k})
		}
	}
	// A base URL that is not waired's own loopback. This is the same test
	// Remove has always applied before deleting the key; Write applies it
	// now too, which is the symmetry #1188 is really about.
	if env, ok := obj["env"].(map[string]any); ok {
		if cur, ok := env[baseURLKey].(string); ok && cur != "" &&
			!strings.HasPrefix(cur, loopbackPrefix) {
			out = append(out, OrgSignal{Key: baseURLKey, Detail: cur})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ErrOrgManaged is what Write returns instead of overwriting. It carries the
// signals so a caller can print them; errors.As is how a caller recognises it
// rather than matching on the message.
type ErrOrgManaged struct {
	Path    string
	Signals []OrgSignal
}

func (e *ErrOrgManaged) Error() string {
	parts := make([]string, 0, len(e.Signals))
	for _, s := range e.Signals {
		parts = append(parts, s.String())
	}
	return fmt.Sprintf("%s is managed by your organisation (%s) — waired left it alone",
		e.Path, strings.Join(parts, ", "))
}

// shortValue renders a JSON value for a one-line message, bounded so a long
// list cannot take over the output.
func shortValue(v any) string {
	s := fmt.Sprintf("%v", v)
	const max = 60
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
