package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// VariantSHA returns a stable digest of one variant's identity, for use
// as a key that means "the same engine bytes on disk".
//
// The digest covers fields that influence what the runtime will actually
// serve (Format, Quantization, DType, VariantSource pointer + revision)
// while excluding mutable maintainer metadata (QualityTier,
// EstimatedWeightGB, RuntimeSupport, MinRAMGB, MinVRAMMB, ParamCount,
// QuantizationTier). Editor-side tuning of those fields therefore does
// NOT change the key.
//
// Renderer and Parser are excluded too, and that one is a compromise
// rather than a classification: they do change what the runtime serves.
// They stay out because the payload below is frozen (see the paragraph
// on that), so widening it would make every persisted measurement stop
// matching everywhere at once — a worse failure than the one being
// prevented. Two variants that differ only in Renderer therefore hash
// alike, and the guard against a silently un-stamped variant lives in
// the measurement stores instead: they record the renderer they were
// taken under and compare it against the manifest on check
// (waired-agent#1192).
//
// Callers composing a CACHE key must add the other identifying inputs
// (GPU model, driver version, engine kind, engine model) — VariantID can
// collide across models, since qwen3-8b and llama3-8b can both ship a
// "q4-gguf" variant, and the digest above deliberately does not carry
// the model id.
//
// It lives in proto because both sides key the same records by it: the
// agent files what it measured under this digest, and the control plane
// has to find the same entry for the same weights. A second spelling of
// this hash would not fail loudly — it would simply never match, and the
// measurements would look absent (waired-agent#784, #970).
//
// The payload shape is frozen. Changing it does not "migrate" anything:
// every persisted key silently stops matching and every host looks like
// it has measured nothing.
func VariantSHA(v Variant) string {
	payload := struct {
		VariantID    string `json:"variant_id"`
		Format       string `json:"format"`
		Quantization string `json:"quantization"`
		DType        string `json:"dtype"`
		SourceType   string `json:"source_type"`
		SourceTag    string `json:"source_tag,omitempty"`
		SourceRepoID string `json:"source_repo_id,omitempty"`
		SourceRev    string `json:"source_revision,omitempty"`
	}{
		VariantID:    v.VariantID,
		Format:       v.Format,
		Quantization: v.Quantization,
		DType:        v.DType,
		SourceType:   v.Source.Type,
		SourceTag:    v.Source.Tag,
		SourceRepoID: v.Source.RepoID,
		SourceRev:    v.Source.Revision,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
