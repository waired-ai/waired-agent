package download

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The file names reach `hf download` as arguments, and they are the Hub's —
// the repository owner's. A name shaped like a flag, or holding a space, is
// left out of the pull (review of waired-ai/waired#1473, 2026-09-22).
func TestListTopLevel_LeavesOutNamesThatAreNotFileNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"type": "file", "path": "model.safetensors", "size": 10},
			{"type": "file", "path": "--dry-run", "size": 1},
			{"type": "file", "path": "-x", "size": 1},
			{"type": "file", "path": "a b.json", "size": 1},
			{"type": "file", "path": "config.json", "size": 2},
			{"type": "file", "path": "sub/model.safetensors", "size": 3},
			{"type": "directory", "path": "fp8"},
		})
	}))
	defer srv.Close()
	got, err := DefaultHFFileLister{BaseURL: srv.URL, HTTP: srv.Client()}.ListTopLevel(context.Background(), "acme/m", "main")
	if err != nil {
		t.Fatal(err)
	}
	names := HFFileNames(got)
	if len(names) != 2 || names[0] != "config.json" || names[1] != "model.safetensors" {
		t.Errorf("names %q, want config.json and model.safetensors only", names)
	}
}

// A custom model pulls only what vLLM loads with --load-format safetensors
// (waired-ai/waired#1480).
func TestCustomHFFiles(t *testing.T) {
	in := []HFRepoFile{
		{Name: "config.json"}, {Name: "generation_config.json"}, {Name: "tokenizer.json"},
		{Name: "tokenizer_config.json"}, {Name: "tokenizer.model"}, {Name: "special_tokens_map.json"},
		{Name: "chat_template.jinja"}, {Name: "model-00001-of-00002.safetensors"},
		{Name: "model.safetensors.index.json"},
		{Name: "pytorch_model.bin"}, {Name: "model.Q4_K_M.gguf"}, {Name: "modeling_custom.py"},
		{Name: "README.md"}, {Name: ".gitattributes"},
	}
	got := HFFileNames(CustomHFFiles(in))
	if len(got) != 9 {
		t.Fatalf("kept %q, want the 9 vLLM loads", got)
	}
	for _, n := range got {
		switch n {
		case "pytorch_model.bin", "model.Q4_K_M.gguf", "modeling_custom.py", "README.md", ".gitattributes":
			t.Errorf("kept %q", n)
		}
	}
	if !HFHasSafetensors(in) || HFHasSafetensors([]HFRepoFile{{Name: "pytorch_model.bin"}}) {
		t.Error("HFHasSafetensors misread a listing")
	}
}
