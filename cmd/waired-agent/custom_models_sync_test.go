package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/notice"
	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

// Custom models on the agent (waired-ai/waired#1473). The sync, the
// withdrawal notice and the Public Share predicate are records of today's
// behaviour, except where a comment cites the owner ruling a case pins.

func customTestManifest(t *testing.T, slug string) catalog.Manifest {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	id, err := protocatalog.MintCustomModelID(slug, protocatalog.CustomIdentity(catalog.RuntimeOllama, "acme/"+slug, slug+".gguf", digest))
	if err != nil {
		t.Fatal(err)
	}
	return catalog.Manifest{
		ModelID: id, DisplayName: "Model " + slug, ContextLength: 40960,
		Runtime: catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID: "q4-k-m", Format: catalog.FormatOllamaTag, Quantization: "Q4_K_M",
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source: catalog.VariantSource{Type: catalog.SourceOllama, Tag: "hf.co/acme/" + slug + ":Q4_K_M",
				Revision: strings.Repeat("b", 40), Digest: digest},
		}},
		ManualOnly: "imported", Provenance: catalog.ProvenanceCustom,
	}
}

type fakeCustomFetcher struct {
	mu    sync.Mutex
	set   catalog.CustomModelSet
	err   error
	calls []string // the revision each call said it had
	done  chan struct{}
}

func (f *fakeCustomFetcher) FetchCustomModels(_ context.Context, deviceID, have string, mk ed25519.PrivateKey) (catalog.CustomModelSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if deviceID == "" || len(mk) != ed25519.PrivateKeySize {
		return catalog.CustomModelSet{}, errors.New("fake: called without the device's identity")
	}
	f.calls = append(f.calls, have)
	if f.done != nil {
		select {
		case f.done <- struct{}{}:
		default:
		}
	}
	return f.set, f.err
}

func TestCustomModelsSyncFetchesOnARevisionItDoesNotHold(t *testing.T) {
	bundled, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	src := catalog.NewCustomSource(filepath.Join(t.TempDir(), "c.json"), "net_a", bundled, nil)
	a := customTestManifest(t, "a")
	f := &fakeCustomFetcher{set: catalog.CustomModelSet{Revision: "r2", Own: []catalog.Manifest{a}}, done: make(chan struct{}, 4)}
	_, mk, _ := ed25519.GenerateKey(nil)
	s := newCustomModelsSync(src, f, "dev_1", mk, func(string) bool { return false }, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Nothing is fetched before the map has carried a revision: a control
	// plane that writes none has no endpoint either.
	s.Kick()
	time.Sleep(50 * time.Millisecond)
	if len(f.calls) != 0 {
		t.Fatalf("fetched before any revision was seen: %v", f.calls)
	}
	s.NoteRevision("r2")
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch after a new revision")
	}
	deadline := time.Now().Add(5 * time.Second)
	for src.Revision() != "r2" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if src.Revision() != "r2" || len(src.Serveable()) != 1 {
		t.Fatalf("set not applied: rev %q, %d serveable", src.Revision(), len(src.Serveable()))
	}
	// The revision it now holds starts nothing.
	s.NoteRevision("r2")
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	n := len(f.calls)
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("%d fetches, want 1", n)
	}
}

func TestCustomModelNoticesNameWhatIsStillInUse(t *testing.T) {
	bundled, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatal(err)
	}
	src := catalog.NewCustomSource(filepath.Join(t.TempDir(), "c.json"), "net_a", bundled, nil)
	a, b := customTestManifest(t, "a"), customTestManifest(t, "b")
	if _, err := src.Replace(catalog.CustomModelSet{Revision: "r1", Own: []catalog.Manifest{a, b}}, nil); err != nil {
		t.Fatal(err)
	}
	prefPath := filepath.Join(t.TempDir(), "preferred-model.json")
	if err := agentconfig.SavePreference(prefPath, agentconfig.Preference{ModelID: a.ModelID, Source: agentconfig.PreferenceSourceDesired}); err != nil {
		t.Fatal(err)
	}
	p := &agentInferenceProvider{custom: src, preferencePath: prefPath, store: catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))}
	if got := p.customModelNotices(); len(got) != 0 {
		t.Fatalf("notices before any deletion: %+v", got)
	}
	// The account deletes both; this computer was told to run a.
	if _, err := src.Replace(catalog.CustomModelSet{Revision: "r2"}, func(id string) bool { return customModelInUseBy(p, id) }); err != nil {
		t.Fatal(err)
	}
	got := p.customModelNotices()
	if len(got) != 1 || got[0].Kind != notice.KindCustomModelWithdrawn || !strings.Contains(got[0].Title, "Model a") {
		t.Fatalf("notices %+v, want one for Model a", got)
	}
	// Public Share guests are refused while it is chosen (ruling 4).
	if !customModelServedOrChosen(&inferenceSubsystem{provider: p}) {
		t.Error("a chosen custom model did not refuse public guests")
	}
	if err := agentconfig.SavePreference(prefPath, agentconfig.Preference{ModelID: bundled[0].ModelID}); err != nil {
		t.Fatal(err)
	}
	if got := p.customModelNotices(); len(got) != 0 {
		t.Errorf("the notice outlived the choice: %+v", got)
	}
	if customModelServedOrChosen(&inferenceSubsystem{provider: p}) {
		t.Error("a bundled choice still refused public guests")
	}
}

// customModelInUseBy is customModelInUse for a bare provider.
func customModelInUseBy(p *agentInferenceProvider, id string) bool {
	return customModelInUse(&inferenceSubsystem{provider: p}, id)
}
