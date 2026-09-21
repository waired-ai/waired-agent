//go:build integration

package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/catalog/scoring"
	"github.com/waired-ai/waired-agent/proto/gguf"
)

// customImportPrefixBytes is how much of a GGUF the control plane reads
// when a person imports one (waired-ai/waired#1476): a Range request for
// the first 16 KiB, which holds the architecture keys ahead of the
// tokenizer arrays.
const customImportPrefixBytes = 16 << 10

// TestGGUFPrefixEstimateMatchesTheCatalog reads the first 16 KiB of every
// bundled ollama build from its registry and prices it the way a custom
// import is priced. Where the estimate is Known it must equal the
// kv_bytes_per_token_fp16 the manifest carries; where it is not, it must
// say why. A bundled build is the best test there is for the importer: the
// catalog's figure was derived from the model's own config.json and
// checked against the engine's log.
func TestGGUFPrefixEstimateMatchesTheCatalog(t *testing.T) {
	manifests, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	c := &ollamaregistry.Client{}
	known, unknown := 0, 0
	for _, m := range manifests {
		for _, v := range m.Variants {
			if v.Format != catalog.FormatOllamaTag || v.KVBytesPerTokenFP16 == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			layers, err := c.Layers(ctx, v.Source.Tag)
			if err != nil {
				cancel()
				t.Errorf("%s/%s: layers of %s: %v", m.ModelID, v.VariantID, v.Source.Tag, err)
				continue
			}
			body, err := c.OpenBlob(ctx, v.Source.Tag, layers.ModelDigest)
			if err != nil {
				cancel()
				t.Errorf("%s/%s: open %s: %v", m.ModelID, v.VariantID, v.Source.Tag, err)
				continue
			}
			h, err := gguf.ReadPrefix(io.LimitReader(body, customImportPrefixBytes))
			body.Close()
			cancel()
			if err != nil {
				t.Errorf("%s/%s: read prefix: %v", m.ModelID, v.VariantID, err)
				continue
			}
			est := scoring.EstimateKVFromGGUF(h)
			if !est.Known {
				unknown++
				if est.Reason == "" {
					t.Errorf("%s/%s: unknown with no reason", m.ModelID, v.VariantID)
				}
				t.Logf("%s/%s (%s): unknown — %s", m.ModelID, v.VariantID, h.Architecture(), est.Reason)
				continue
			}
			known++
			if est.BytesPerTokenFP16 != v.KVBytesPerTokenFP16 {
				t.Errorf("%s/%s (%s): the 16 KiB estimate is %d B/token, the manifest carries %d",
					m.ModelID, v.VariantID, h.Architecture(), est.BytesPerTokenFP16, v.KVBytesPerTokenFP16)
			}
			if est.ContextLength == 0 {
				t.Errorf("%s/%s: no context_length in the first 16 KiB", m.ModelID, v.VariantID)
			}
		}
	}
	t.Logf("%d bundled ollama builds priced from 16 KiB, %d unknown", known, unknown)
	if known == 0 {
		t.Fatal("no bundled build was priced")
	}
}
