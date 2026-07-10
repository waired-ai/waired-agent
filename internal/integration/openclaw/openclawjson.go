package openclaw

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// modelRefs are the three picker references the adapter allowlists in
// agents.defaults.models so the waired models surface in `models list` and
// the model picker. They match the plugin's resolveDynamicModel keys and
// the gateway catalog aliases.
func modelRefs() []string {
	return []string{"waired/default", "waired/coding", "waired/small"}
}

// legacyModelRefs are model picker references this integration used to add
// under agents.defaults.models but no longer owns (waired/auto was renamed to
// waired/default). mergeConfig deletes them on re-apply and removeManagedKeys
// deletes them on uninstall, so a re-link after upgrade fully overwrites a
// stale entry rather than leaving it orphaned in the user's config.
func legacyModelRefs() []string {
	return []string{"waired/auto"}
}

// managedAddedPaths is the fixed, human-readable list of dotted openclaw.json
// paths Apply owns, recorded in the ledger for `waired doctor` context. The
// actual removal is keyed on the concrete plugin dir + model refs, not on
// parsing these strings back.
func managedAddedPaths() []string {
	out := []string{"plugins.load.paths[waired]", "plugins.entries.waired"}
	for _, r := range modelRefs() {
		out = append(out, "agents.defaults.models["+r+"]")
	}
	return out
}

// readConfigObject reads openclaw.json into an ordered-agnostic map. The
// second return is false when the file does not exist (a fresh OpenClaw
// install that has never run `openclaw setup`).
func readConfigObject(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, false, nil
		}
		return nil, false, fmt.Errorf("openclaw: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, true, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, true, fmt.Errorf("openclaw: parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true, nil
}

// childMap returns parent[key] as a map, creating an empty one when absent.
// It returns an error if the existing value is present but not a JSON
// object, so we never clobber unexpected user data.
func childMap(parent map[string]any, key string) (map[string]any, error) {
	v, ok := parent[key]
	if !ok || v == nil {
		m := map[string]any{}
		parent[key] = m
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openclaw: %q is not a JSON object", key)
	}
	return m, nil
}

// childMapNoCreate returns parent[key] as a map, or nil when absent / not an
// object (removal is best-effort and must not fail on a hand-edited file).
func childMapNoCreate(parent map[string]any, key string) map[string]any {
	if v, ok := parent[key]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return nil
}

// stringSlice coerces a JSON array value into a []string, dropping non-string
// entries. A nil / non-array value yields an empty slice.
func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mergeConfig inserts the waired-owned keys into the parsed config: appends
// pluginDir to plugins.load.paths, sets plugins.entries.waired.enabled, and
// allowlists the model refs under agents.defaults.models. Idempotent.
func mergeConfig(m map[string]any, pluginDir string) error {
	plugins, err := childMap(m, "plugins")
	if err != nil {
		return err
	}
	load, err := childMap(plugins, "load")
	if err != nil {
		return err
	}
	paths := stringSlice(load["paths"])
	if !containsString(paths, pluginDir) {
		paths = append(paths, pluginDir)
	}
	load["paths"] = toAnySlice(paths)

	entries, err := childMap(plugins, "entries")
	if err != nil {
		return err
	}
	entries["waired"] = map[string]any{"enabled": true}

	agents, err := childMap(m, "agents")
	if err != nil {
		return err
	}
	defaults, err := childMap(agents, "defaults")
	if err != nil {
		return err
	}
	models, err := childMap(defaults, "models")
	if err != nil {
		return err
	}
	for _, ref := range modelRefs() {
		if _, ok := models[ref]; !ok {
			models[ref] = map[string]any{}
		}
	}
	// Prune refs we used to own but renamed away from, so a re-link after an
	// upgrade fully overwrites a stale entry instead of leaving it orphaned.
	for _, ref := range legacyModelRefs() {
		delete(models, ref)
	}
	return nil
}

// removeManagedKeys strips exactly the keys mergeConfig added, leaving any
// peer plugins / models the user owns intact and pruning empty parent
// objects. Best-effort: a hand-edited file with unexpected types is left as
// much intact as possible.
func removeManagedKeys(m map[string]any, pluginDir string) {
	if plugins := childMapNoCreate(m, "plugins"); plugins != nil {
		if load := childMapNoCreate(plugins, "load"); load != nil {
			paths := removeString(stringSlice(load["paths"]), pluginDir)
			if len(paths) == 0 {
				delete(load, "paths")
			} else {
				load["paths"] = toAnySlice(paths)
			}
			if len(load) == 0 {
				delete(plugins, "load")
			}
		}
		if entries := childMapNoCreate(plugins, "entries"); entries != nil {
			delete(entries, "waired")
			if len(entries) == 0 {
				delete(plugins, "entries")
			}
		}
		if len(plugins) == 0 {
			delete(m, "plugins")
		}
	}
	if agents := childMapNoCreate(m, "agents"); agents != nil {
		if defaults := childMapNoCreate(agents, "defaults"); defaults != nil {
			if models := childMapNoCreate(defaults, "models"); models != nil {
				for _, ref := range modelRefs() {
					delete(models, ref)
				}
				for _, ref := range legacyModelRefs() {
					delete(models, ref)
				}
				if len(models) == 0 {
					delete(defaults, "models")
				}
			}
			if len(defaults) == 0 {
				delete(agents, "defaults")
			}
		}
		if len(agents) == 0 {
			delete(m, "agents")
		}
	}
}

// marshalConfig serialises the config map with stable 2-space indentation
// and a trailing newline. Keys are emitted in sorted order (json default);
// the original key order is not preserved, which is why Apply takes a
// backup before mutating an existing file.
func marshalConfig(m map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("openclaw: marshal config: %w", err)
	}
	return append(body, '\n'), nil
}

// backupConfig copies path to "<path>.waired-bak-<unix-ts>" before the first
// mutation. Returns the backup path (empty when path did not exist).
func backupConfig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("openclaw: read for backup %s: %w", path, err)
	}
	bak := fmt.Sprintf("%s.waired-bak-%d", path, time.Now().Unix())
	if err := os.WriteFile(bak, data, 0o644); err != nil {
		return "", fmt.Errorf("openclaw: write backup %s: %w", bak, err)
	}
	return bak, nil
}

// isEffectivelyEmpty reports whether the config map serialises to an empty
// object, so Uninstall can delete a file it fully owned.
func isEffectivelyEmpty(m map[string]any) bool {
	return len(m) == 0
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func removeString(ss []string, drop string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func toAnySlice(ss []string) []any {
	// Sort for deterministic output (paths order is not significant to
	// OpenClaw and a stable order keeps re-applies diff-free).
	sorted := append([]string(nil), ss...)
	sort.Strings(sorted)
	out := make([]any, len(sorted))
	for i, s := range sorted {
		out[i] = s
	}
	return out
}
