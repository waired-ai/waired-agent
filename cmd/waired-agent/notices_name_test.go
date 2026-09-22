package main

import (
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A custom model's "did not start" notice names it by the name the person
// gave it in the console, not by its minted id; a model from Waired's list
// keeps its id (waired-ai/waired#1482, found on real hardware). Record of
// today's behaviour.
func TestLoadFailureNotice_NamesACustomModelByItsName(t *testing.T) {
	p := vllmSwapProvider(t)
	m := vllmSwapManifests()[0]
	m.ModelID, m.DisplayName, m.Provenance = "custom-tiny-0123abcd", "Tiny model", catalog.ProvenanceCustom
	p.manifests = append(p.manifests, m)
	v := m.Variants[1]
	shape := vllmLoadShape(infruntime.ModelTuning{ContextLength: 40960}, "", 4)
	hint := vllmStartupDiagnosis(vllmLogNoWeights, "127.0.0.1:9510")
	p.recordVLLMLoadFailure(context.Background(), m, v, shape, hint, "", signer.LoadFailureWeightsMissing, 0)

	ns := p.loadFailureNotices(context.Background())
	if len(ns) != 1 || !strings.HasPrefix(ns[0].Title, "Tiny model did not start") ||
		strings.Contains(ns[0].Title+ns[0].Text, m.ModelID) {
		t.Errorf("notices %+v, want the display name and not the id", ns)
	}
	if got := p.noticeModelName(vllmSwapManifests()[0].ModelID); got != vllmSwapManifests()[0].ModelID {
		t.Errorf("a listed model is named %q, want its id", got)
	}
	if got := p.noticeModelName("custom-gone-0123abcd"); got != "custom-gone-0123abcd" {
		t.Errorf("a custom model this computer does not hold is named %q, want its id", got)
	}
}
