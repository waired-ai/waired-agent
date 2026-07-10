package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// VariantSHA returns a stable digest of one variant's identity for use
// as a cache key (e.g. the boot-time benchmark cache, where the caller
// wants "same engine bytes on disk → same key").
//
// The digest covers fields that influence what the runtime will
// actually serve (Format, Quantization, DType, VariantSource pointer +
// revision) while excluding mutable maintainer metadata (QualityTier,
// EstimatedWeightGB, RuntimeSupport, MinRAMGB, MinVRAMMB, ParamCount,
// QuantizationTier). Editor-side tuning of those fields therefore does
// NOT invalidate cache entries.
//
// Callers must compose VariantSHA with other identifying inputs
// (GPU model, driver version, engine kind, engine model) before using
// the result as a key — VariantID can collide across models (qwen3-8b
// and llama3-8b can both ship a "q4-gguf" variant).
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
