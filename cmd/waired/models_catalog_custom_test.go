package main

import (
	"strings"
	"testing"
)

// TestFormatCatalogDetailGroupsCustomModels is a record of today's
// behaviour: the custom models the daemon lists after the catalog's
// (waired-ai/waired#1473) sit under one heading line in `models ls
// --detail`, and a catalog with none prints no heading.
func TestFormatCatalogDetailGroupsCustomModels(t *testing.T) {
	c := catalogDetailResp{Engine: "ollama", Families: []catalogDetailFamily{
		{ModelID: "qwen3-8b-instruct", Fits: true},
		{ModelID: "custom-tiny-0123abcd", Custom: true, Fits: true},
	}}
	out := formatCatalogDetail(c)
	head := strings.Index(out, "Custom models")
	bundled := strings.Index(out, "qwen3-8b-instruct")
	custom := strings.Index(out, "custom-tiny-0123abcd")
	if head < 0 || bundled >= head || head >= custom {
		t.Errorf("heading at %d, bundled at %d, custom at %d:\n%s", head, bundled, custom, out)
	}
	if strings.Count(out, "Custom models") != 1 {
		t.Errorf("heading printed %d times", strings.Count(out, "Custom models"))
	}
	c.Families = c.Families[:1]
	if strings.Contains(formatCatalogDetail(c), "Custom models") {
		t.Error("a catalog with no custom model has the heading")
	}
}
