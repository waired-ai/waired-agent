package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Custom models are models a person imports from Hugging Face through
// the console and runs on their own computers (waired-ai/waired#1473).
// They reach an agent as ordinary Manifests, fetched from the control
// plane rather than embedded, and the rules below are what separate them
// from the bundled catalog. The owner's rulings are recorded in the
// private monorepo (docs/decisions/20260922/0300-custom-model-rulings.md);
// the wire contract is the 2026-09-22 comment on waired-ai/waired#1475.

// ProvenanceCustom is Manifest.Provenance for a model a person imported.
// Bundled manifests leave Provenance empty.
const ProvenanceCustom = "custom"

// CustomModelIDPrefix starts every custom model id. No bundled id, alias
// or retired name may start with it (TestBundledNamesAvoidTheCustomPrefix),
// so the prefix alone tells the two apart.
const CustomModelIDPrefix = "custom-"

// MaxCustomModelIDBytes bounds a custom model id. The control plane keeps
// a device's active model in 64 bytes, and an id longer than that would
// fail every status report the device sends.
const MaxCustomModelIDBytes = 64

// maxCustomSlugBytes leaves room in MaxCustomModelIDBytes for the prefix
// and the "-" + 8-hex identity suffix: 7 + 48 + 1 + 8 = 64.
const maxCustomSlugBytes = 48

// customIdentityVersion is folded into every identity so that a later
// change to what identifies a model can mint a new space of ids instead of
// colliding with this one.
const customIdentityVersion = "waired.custom.v1"

var (
	customModelIDRe  = regexp.MustCompile(`^custom-[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?-[0-9a-f]{8}$`)
	hfNameRe         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	hfQuantRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	commitSHARe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	engineTokenRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	vllmParserNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
)

// maxCustomDisplayNameRunes bounds the name a person gives a custom model.
const maxCustomDisplayNameRunes = 64

// maxCustomContextLength bounds ContextLength on a custom manifest. The
// value is read from the model's own files; anything past this is a
// corrupt header rather than a model.
const maxCustomContextLength = 16 << 20

// IsCustomModelID reports whether id names a custom model. It checks only
// the prefix: whether the id is well formed is ValidCustomModelID.
func IsCustomModelID(id string) bool {
	return strings.HasPrefix(id, CustomModelIDPrefix)
}

// ValidCustomModelID reports whether id has the exact shape
// MintCustomModelID produces: the prefix, a lowercase slug, a hyphen and
// eight hex digits, at most MaxCustomModelIDBytes long. It never contains
// "/", which keeps a custom id out of every path-element match.
func ValidCustomModelID(id string) bool {
	return len(id) <= MaxCustomModelIDBytes && customModelIDRe.MatchString(id) && !strings.Contains(id, "..")
}

// CustomIdentity is what makes two imports the same model: the engine,
// the canonical repository id (compared without case, as Hugging Face
// resolves it), the file, and the digest of that file's content. On vLLM
// the file is "" and the digest is the pinned commit SHA. Two imports with
// the same identity get the same id whoever imports them; a file replaced
// upstream under the same name has a new digest and so is a different
// model (owner ruling 7, 2026-09-22).
//
// The result is the lowercase hex SHA-256 of the fields, each followed by a
// NUL, after a version string.
func CustomIdentity(engine, repoID, file, digest string) string {
	h := sha256.New()
	for _, part := range []string{customIdentityVersion, engine, strings.ToLower(repoID), file, strings.ToLower(digest)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MintCustomModelID builds a custom model id from a human-readable slug
// source (a repository name, with the quantization for ollama) and a
// CustomIdentity. The slug is lowercased, every run of characters outside
// [a-z0-9.] becomes one "-", and it is cut to fit; the first eight hex
// digits of the identity follow it.
func MintCustomModelID(slugSource, identity string) (string, error) {
	if len(identity) < 8 {
		return "", fmt.Errorf("custom model id: identity %q is shorter than 8 hex digits", identity)
	}
	for _, c := range identity[:8] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("custom model id: identity %q is not lowercase hex", identity)
		}
	}
	slug := customSlug(slugSource)
	if slug == "" {
		slug = "model"
	}
	id := CustomModelIDPrefix + slug + "-" + identity[:8]
	if !ValidCustomModelID(id) {
		return "", fmt.Errorf("custom model id: %q is not a valid id", id)
	}
	return id, nil
}

func customSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	if len(out) > maxCustomSlugBytes {
		out = out[:maxCustomSlugBytes]
	}
	return strings.Trim(out, "-.")
}

// CustomModelSet is what the control plane returns to an agent that asks
// for its account's custom models. Own holds the complete manifests of the
// device owner's imports; Team holds routing-only copies of what the
// owner's teammates imported (ValidateCustomProjection). An id present in
// both is the owner's own. Revision echoes the value the control plane put
// on the device's own network-map entry (InferenceState.CustomModelsRevision).
type CustomModelSet struct {
	Revision string     `json:"revision,omitempty"`
	Own      []Manifest `json:"own,omitempty"`
	Team     []Manifest `json:"team,omitempty"`
}

// ValidateCustomManifest checks a custom model's manifest before anything
// of it reaches a Modelfile, an engine's argv, or a directory name. It runs
// the structural checks Validate runs, with the three numbers a person
// cannot supply allowed to be 0 ("unknown", the lowest rank), and adds the
// rules that keep a custom model apart from the bundled catalog.
//
// bundled is the catalog to keep clear of: every bundled manifest,
// including internal ones.
func ValidateCustomManifest(m Manifest, bundled []Manifest) error {
	if err := validateCustomCommon(m, bundled); err != nil {
		return err
	}
	if err := m.validate(true); err != nil {
		return err
	}
	v := m.Variants[0]
	if v.MTPLayers != 0 || v.MTPKVBytesPerTokenFP16 != 0 || v.MTPDraftTokens != 0 {
		return fmt.Errorf("custom model %s: multi-token prediction is not carried on a custom model", m.ModelID)
	}
	if err := validCustomLicense(m.License); err != nil {
		return fmt.Errorf("custom model %s: %w", m.ModelID, err)
	}
	if !customVLLMDTypes[v.DType] {
		return fmt.Errorf("custom model %s: dtype %q is not one vLLM accepts", m.ModelID, v.DType)
	}
	if err := validateCustomSource(m.ModelID, v, false); err != nil {
		return err
	}
	for _, r := range []struct{ name, val string }{{"renderer", v.Renderer}, {"parser", v.Parser}} {
		if r.val != "" && !engineTokenRe.MatchString(r.val) {
			return fmt.Errorf("custom model %s: %s %q is not a plain engine name", m.ModelID, r.name, r.val)
		}
	}
	if (v.VLLMToolCallParser != "" || v.VLLMReasoningParser != "") && !runtimesEqual(v.RuntimeSupport, []string{RuntimeVLLM}) {
		return fmt.Errorf("custom model %s: vLLM parsers are set on a build vLLM does not serve", m.ModelID)
	}
	for _, r := range []struct{ name, val string }{{"vllm_tool_call_parser", v.VLLMToolCallParser}, {"vllm_reasoning_parser", v.VLLMReasoningParser}} {
		if r.val != "" && !vllmParserNameRe.MatchString(r.val) {
			return fmt.Errorf("custom model %s: %s %q is not a parser name", m.ModelID, r.name, r.val)
		}
	}
	return nil
}

// ValidateCustomProjection checks the routing-only copy of a teammate's
// custom model. It holds the same id and name rules as
// ValidateCustomManifest, and refuses every field a router does not need:
// what pins, stamps or starts the model stays with the account that
// imported it.
func ValidateCustomProjection(m Manifest, bundled []Manifest) error {
	if err := validateCustomCommon(m, bundled); err != nil {
		return err
	}
	v := m.Variants[0]
	if v.Source.Digest != "" || v.Source.Revision != "" || v.Renderer != "" || v.Parser != "" ||
		v.VLLMToolCallParser != "" || v.VLLMReasoningParser != "" || v.GGUF != nil {
		return fmt.Errorf("custom model %s: a teammate's copy carries only what routing reads", m.ModelID)
	}
	return validateCustomSource(m.ModelID, v, true)
}

// validateCustomCommon holds the rules a custom manifest and a teammate's
// copy share: provenance, id, name, the one variant, and no reuse of any
// name the bundled catalog or its retirements hold.
func validateCustomCommon(m Manifest, bundled []Manifest) error {
	if m.Provenance != ProvenanceCustom {
		return fmt.Errorf("custom model %s: provenance is %q, want %q", m.ModelID, m.Provenance, ProvenanceCustom)
	}
	if !ValidCustomModelID(m.ModelID) {
		return fmt.Errorf("custom model %q: not a custom model id", m.ModelID)
	}
	if err := validCustomDisplayName(m.DisplayName); err != nil {
		return fmt.Errorf("custom model %s: %w", m.ModelID, err)
	}
	if m.ManualOnly == "" {
		return fmt.Errorf("custom model %s: manual_only is required (a custom model is never chosen automatically)", m.ModelID)
	}
	if m.InternalOnly != "" {
		return fmt.Errorf("custom model %s: internal_only is not for custom models", m.ModelID)
	}
	if len(m.ModelAliases) != 0 {
		return fmt.Errorf("custom model %s: a custom model has no aliases", m.ModelID)
	}
	if m.RopeScaling != nil {
		return fmt.Errorf("custom model %s: rope_scaling is not carried on a custom model", m.ModelID)
	}
	if m.Security.TrustRemoteCodeRequired {
		return fmt.Errorf("custom model %s: a model that needs remote code cannot be imported", m.ModelID)
	}
	if len(m.Variants) != 1 {
		return fmt.Errorf("custom model %s: exactly one variant, got %d", m.ModelID, len(m.Variants))
	}
	if m.ContextLength <= 0 || m.ContextLength > maxCustomContextLength {
		return fmt.Errorf("custom model %s: context_length %d is out of range", m.ModelID, m.ContextLength)
	}
	if len(m.DefaultVariant) != 0 {
		return fmt.Errorf("custom model %s: default_variant is not used on a custom model", m.ModelID)
	}
	return customNameClash(m, bundled)
}

func validCustomDisplayName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("display_name is required")
	}
	if !utf8.ValidString(name) {
		return errors.New("display_name is not UTF-8")
	}
	if utf8.RuneCountInString(name) > maxCustomDisplayNameRunes {
		return fmt.Errorf("display_name is longer than %d characters", maxCustomDisplayNameRunes)
	}
	if strings.TrimSpace(name) != name {
		return errors.New("display_name has leading or trailing space")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("display_name contains a control character")
		}
		// Format characters (zero-width spaces and joiners, bidi
		// overrides and isolates), private-use characters and the line
		// and paragraph separators render as nothing or reorder the
		// text around them, so a name holding one can look exactly like
		// a bundled model's or a teammate's (review of
		// waired-ai/waired#1473, 2026-09-22).
		if unicode.In(r, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp) {
			return errors.New("display_name contains an invisible or formatting character")
		}
	}
	return nil
}

// ValidateCustomDisplayName checks a display name by the rules
// ValidateCustomManifest applies, so the control plane can tell a person
// their name is the problem before it builds anything.
func ValidateCustomDisplayName(name string) error {
	return validCustomDisplayName(name)
}

// CustomNameTaken reports whether name is already a bundled model's id,
// alias or display name, or a retired name, compared as CustomNameKey
// compares — the clash ValidateCustomManifest refuses. owner is the bundled
// id (or "retired <name>") that holds it.
func CustomNameTaken(name string, bundled []Manifest) (owner string, taken bool) {
	owner, taken = bundledNames(bundled)[CustomNameKey(name)]
	return owner, taken
}

// CustomNameKey is the form two custom-model names are compared in: without
// case, and with every run of whitespace (any Unicode space, including the
// full-width one) read as one ASCII space. "Qwen3 8B" and "qwen3  8b" are
// the same name to a person reading a list, so they are the same name here
// — for the bundled-name clash and for the control plane's per-account
// uniqueness alike.
func CustomNameKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// maxCustomLicenseBytes bounds Manifest.License on a custom model. The
// control plane copies it from the model card, which is free text of any
// size; a license identifier ("apache-2.0", "llama3.1", "other") is short.
const maxCustomLicenseBytes = 64

func validCustomLicense(license string) error {
	if len(license) > maxCustomLicenseBytes {
		return fmt.Errorf("license is longer than %d bytes", maxCustomLicenseBytes)
	}
	for i := 0; i < len(license); i++ {
		if c := license[i]; c < 0x20 || c > 0x7e {
			return errors.New("license is not printable ASCII")
		}
	}
	return nil
}

// customVLLMDTypes are the --dtype values a custom vLLM build may carry.
// The control plane sets none today; the list is what vLLM's own flag
// accepts, so the agent never passes it anything else.
var customVLLMDTypes = map[string]bool{
	"": true, "auto": true, "half": true, "float16": true, "bfloat16": true, "float": true, "float32": true,
}

// customNameClash refuses a custom id or display name that any bundled
// id, alias or display name, or any retired name, already holds. Names are
// compared without case: resolution treats them as the same name to a
// person reading a list.
func customNameClash(m Manifest, bundled []Manifest) error {
	taken := bundledNames(bundled)
	for _, name := range []string{m.ModelID, m.DisplayName} {
		if owner, ok := taken[CustomNameKey(name)]; ok {
			return fmt.Errorf("custom model %s: %q is already a bundled or retired name (%s)", m.ModelID, name, owner)
		}
	}
	return nil
}

// bundledNames maps the CustomNameKey of every bundled id, alias and display
// name, and of every retired name, to what holds it.
func bundledNames(bundled []Manifest) map[string]string {
	taken := map[string]string{}
	for _, b := range bundled {
		taken[CustomNameKey(b.ModelID)] = b.ModelID
		if b.DisplayName != "" {
			taken[CustomNameKey(b.DisplayName)] = b.ModelID
		}
		for _, a := range b.ModelAliases {
			taken[CustomNameKey(a)] = b.ModelID
		}
	}
	for _, r := range Retirements() {
		for _, n := range r.Names {
			taken[CustomNameKey(n)] = "retired " + n
		}
	}
	return taken
}

// validateCustomSource holds a custom variant to the two shapes an import
// can produce. projection relaxes the pins a teammate's copy leaves out.
func validateCustomSource(modelID string, v Variant, projection bool) error {
	switch {
	case runtimesEqual(v.RuntimeSupport, []string{RuntimeOllama}):
		if v.Format != FormatOllamaTag || v.Source.Type != SourceOllama {
			return fmt.Errorf("custom model %s: an ollama build is format=ollama-tag with source.type=ollama", modelID)
		}
		if _, _, ok := ParseHFTag(v.Source.Tag); !ok {
			return fmt.Errorf("custom model %s: source.tag %q is not hf.co/<org>/<repo>:<quantization>", modelID, v.Source.Tag)
		}
		if v.Source.RepoID != "" {
			return fmt.Errorf("custom model %s: an ollama build carries no source.repo_id", modelID)
		}
		if projection {
			return nil
		}
		if !validDigest(v.Source.Digest) {
			return fmt.Errorf("custom model %s: source.digest %q is not sha256:<64 lowercase hex>", modelID, v.Source.Digest)
		}
		if !commitSHARe.MatchString(v.Source.Revision) {
			return fmt.Errorf("custom model %s: source.revision %q is not a 40-hex commit", modelID, v.Source.Revision)
		}
	case runtimesEqual(v.RuntimeSupport, []string{RuntimeVLLM}):
		if v.Format != FormatSafetensors || v.Source.Type != SourceHuggingFace {
			return fmt.Errorf("custom model %s: a vLLM build is format=safetensors with source.type=huggingface", modelID)
		}
		if !ValidHFRepoID(v.Source.RepoID) {
			return fmt.Errorf("custom model %s: source.repo_id %q is not <org>/<repo>", modelID, v.Source.RepoID)
		}
		if v.Source.Tag != "" || v.Source.Digest != "" {
			return fmt.Errorf("custom model %s: a vLLM build carries no source.tag or source.digest", modelID)
		}
		if projection {
			return nil
		}
		if !commitSHARe.MatchString(v.Source.Revision) {
			return fmt.Errorf("custom model %s: source.revision %q is not a 40-hex commit", modelID, v.Source.Revision)
		}
	default:
		return fmt.Errorf("custom model %s: runtime_support must be exactly [ollama] or [vllm], got %v", modelID, v.RuntimeSupport)
	}
	return nil
}

// ValidHFRepoID reports whether id is a Hugging Face "<org>/<repo>" in the
// characters the Hub allows, with no path traversal. It is what a custom
// model's repository may look like before it becomes a directory name or
// an argument.
func ValidHFRepoID(id string) bool {
	org, repo, ok := strings.Cut(id, "/")
	return ok && hfNameRe.MatchString(org) && hfNameRe.MatchString(repo) &&
		!strings.Contains(org, "..") && !strings.Contains(repo, "..")
}

// ParseHFTag splits an ollama tag of the form "hf.co/<org>/<repo>:<quant>"
// into its repository id and quantization. ok is false for any other shape.
func ParseHFTag(tag string) (repoID, quant string, ok bool) {
	rest, found := strings.CutPrefix(tag, "hf.co/")
	if !found {
		return "", "", false
	}
	repoID, quant, found = strings.Cut(rest, ":")
	if !found || !ValidHFRepoID(repoID) || !hfQuantRe.MatchString(quant) {
		return "", "", false
	}
	return repoID, quant, true
}

// BundledSourceMatch reports which bundled model already serves the same
// weights an import names: an ollama build with the same tag, or a vLLM
// build of the same repository (compared without case). An import that
// matches is refused, because peers match a model by its engine tag and a
// custom model with a bundled tag could not be told apart from the bundled
// one on the wire.
func BundledSourceMatch(engine, tagOrRepo string, bundled []Manifest) (modelID string, ok bool) {
	for _, m := range bundled {
		for _, v := range m.Variants {
			switch {
			case engine == RuntimeOllama && v.Source.Type == SourceOllama && strings.EqualFold(v.Source.Tag, tagOrRepo):
				return m.ModelID, true
			case engine == RuntimeVLLM && v.Source.Type == SourceHuggingFace && strings.EqualFold(v.Source.RepoID, tagOrRepo):
				return m.ModelID, true
			}
		}
	}
	return "", false
}
