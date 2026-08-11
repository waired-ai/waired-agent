package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// pickerFakeDaemon serves the three routes the model picker touches,
// recording every write so the tests can assert what the daemon was
// actually told (repo rule: fakes take and record the real arguments).
type pickerFakeDaemon struct {
	mu sync.Mutex
	// catalogs is served in order, one per GET; the last repeats. The
	// picker fetches once for the list and once per live re-check, so a
	// two-element slice is "the world changed between list and confirm".
	catalogs []catalogDetailResp
	noCat    bool
	gets     int
	// preferred records each POST /inference/preferred-model body, raw.
	preferred []string
	// pendings records each POST /inference/model-choice-pending claim.
	pendings []bool
}

func (f *pickerFakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/waired/v1/inference/catalog":
			if f.noCat || len(f.catalogs) == 0 {
				http.NotFound(w, r)
				return
			}
			i := f.gets
			if i >= len(f.catalogs) {
				i = len(f.catalogs) - 1
			}
			f.gets++
			_ = json.NewEncoder(w).Encode(f.catalogs[i])
		case "/waired/v1/inference/preferred-model":
			b, _ := io.ReadAll(r.Body)
			f.preferred = append(f.preferred, string(b))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"model_id":"","will_restart":false}`))
		case "/waired/v1/inference/model-choice-pending":
			var req struct {
				Pending bool `json:"pending"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.pendings = append(f.pendings, req.Pending)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *pickerFakeDaemon) preferredBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.preferred...)
}

func (f *pickerFakeDaemon) pendingClaims() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.pendings...)
}

func pickerCatalog() catalogDetailResp {
	var c catalogDetailResp
	c.Families = []catalogDetailFamily{
		{ModelID: "qwen3.5-2b", Fits: true},
		{ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, RecommendedPick: true},
		{ModelID: "qwen3.5-27b", Fits: false, DeficitLabel: "needs 24 GB RAM (have 16 GB)"},
	}
	return c
}

// PRODUCT CONTRACT (waired-agent#586; owner-ruled 2026-08-08,
// waired-ai/waired#1067): Enter takes the recommended pick — and the
// default is the recommended ROW, not row 1, so the prompt must carry
// its number.
func TestModelPicker_EnterTakesTheRecommended(t *testing.T) {
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", eofLineReader(), &out, mine)

	if got.picked != "qwen3.5-9b" || got.none {
		t.Fatalf("outcome = %+v, want the recommended qwen3.5-9b", got)
	}
	bodies := f.preferredBodies()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"model_id":"qwen3.5-9b"`) {
		t.Errorf("preferred-model bodies = %v, want one naming qwen3.5-9b", bodies)
	}
	o := out.String()
	for _, want := range []string{
		"Choose the AI model for this computer (Enter = recommended):",
		// The display name, not the id: every install-flow surface that
		// names a model spells it the same way (waired-agent#649).
		"2) Qwen3.5 9B — recommended for this computer",
		"3) qwen3.5-27b — needs 24 GB RAM (have 16 GB)",
		"0) Don't download a model now",
		"Model [2]: ",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("output missing %q:\n%s", want, o)
		}
	}
}

// PRODUCT CONTRACT (#586): "0" completes init with the engine installed
// and no model — a normal state, with the approved completion copy.
func TestModelPicker_ZeroIsTheNoneChoice(t *testing.T) {
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

	if !got.none || got.picked != "" {
		t.Fatalf("outcome = %+v, want none", got)
	}
	bodies := f.preferredBodies()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"none":true`) {
		t.Errorf("preferred-model bodies = %v, want one {\"none\":true}", bodies)
	}
	o := out.String()
	if !strings.Contains(o, "No model selected — the AI software stays ready.") ||
		!strings.Contains(o, "Pick one later with `waired models pull <model>` or from the browser dashboard.") {
		t.Errorf("missing the approved completion copy:\n%s", o)
	}
}

// PRODUCT CONTRACT (#586, sharing #592's confirmed copy): an unfit pick
// warns with the deficit and asks, default No — and No returns to the
// list rather than ending the step.
func TestModelPicker_UnfitPickNoReturnsToTheList(t *testing.T) {
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("3\nn\n1\n"), &out, mine)

	if got.picked != "qwen3.5-2b" {
		t.Fatalf("outcome = %+v, want qwen3.5-2b after declining the unfit pick", got)
	}
	o := out.String()
	if !strings.Contains(o, "does not fit in this computer's memory: needs 24 GB RAM (have 16 GB).") ||
		!strings.Contains(o, "Loading it is expected to fail after the download completes.") ||
		!strings.Contains(o, "Download it anyway?") || !strings.Contains(o, "(default: No)") {
		t.Errorf("missing the #592 unfit confirm:\n%s", o)
	}
	if strings.Count(o, "Choose the AI model for this computer") != 2 {
		t.Errorf("No must return to the list (want it rendered twice):\n%s", o)
	}
	if bodies := f.preferredBodies(); len(bodies) != 1 || !strings.Contains(bodies[0], "qwen3.5-2b") {
		t.Errorf("preferred-model bodies = %v, want only the final qwen3.5-2b", bodies)
	}
}

// PRODUCT CONTRACT (waired-ai/waired#1067 R5, soft limits): Yes is
// honoured — no surface refuses a model any more.
func TestModelPicker_UnfitPickYesIsHonoured(t *testing.T) {
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("3\ny\n"), &out, mine)

	if got.picked != "qwen3.5-27b" {
		t.Fatalf("outcome = %+v, want the unfit qwen3.5-27b honoured on Yes", got)
	}
}

// The runs-but-demoted row confirms through #592's other arm
// ("Use it anyway?", default No) instead of the unfit one.
func TestModelPicker_NotRecommendedPickConfirms(t *testing.T) {
	cat := pickerCatalog()
	cat.Families[0].Fit = &catalogDetailFit{Runnable: true, NotRecommended: true, NotRecommendedReason: "weights_spill"}
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("1\ny\n"), &out, mine)

	if got.picked != "qwen3.5-2b" {
		t.Fatalf("outcome = %+v, want qwen3.5-2b honoured on Yes", got)
	}
	o := out.String()
	if !strings.Contains(o, "runs on this computer, but is not recommended here") ||
		!strings.Contains(o, "Use it anyway?") {
		t.Errorf("missing the not-recommended confirm:\n%s", o)
	}
}

// PRODUCT CONTRACT (#586 + waired-ai/waired#1067 R5): on a host where
// nothing is recommended the default is "0) don't download a model now"
// — the auto-selection would pick nothing there either, and Enter must
// not mean a download the machine cannot hold.
func TestModelPicker_NothingRecommendedDefaultsToNone(t *testing.T) {
	var cat catalogDetailResp
	cat.Families = []catalogDetailFamily{
		{ModelID: "qwen3.5-9b", Fits: false, DeficitLabel: "needs 12 GB RAM (have 7 GB)"},
	}
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", eofLineReader(), &out, mine)

	if !got.none {
		t.Fatalf("outcome = %+v, want none by default", got)
	}
	o := out.String()
	if !strings.Contains(o, "Model [0]: ") {
		t.Errorf("prompt must show 0 as the default:\n%s", o)
	}
	if strings.Contains(o, "(Enter = recommended)") {
		t.Errorf("header must not promise a recommendation that does not exist:\n%s", o)
	}
}

// The live re-check re-reads the catalog at confirm time: a verdict that
// moved between the list and the answer (an engine loaded, a VM
// started) is the one the confirm shows (#586).
func TestModelPicker_LiveRecheckUsesTheFreshVerdict(t *testing.T) {
	stale := pickerCatalog()
	stale.Families[0].Fits = true // listed as fitting…
	fresh := pickerCatalog()
	fresh.Families[0].Fits = false // …but does not fit by confirm time
	fresh.Families[0].DeficitLabel = "needs 6 GB RAM (have 5 GB)"
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{stale, fresh}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("1\nn\n0\n"), &out, mine)

	if !got.none {
		t.Fatalf("outcome = %+v, want none after declining the re-checked pick", got)
	}
	if !strings.Contains(out.String(), "needs 6 GB RAM (have 5 GB)") {
		t.Errorf("the confirm must show the FRESH verdict:\n%s", out.String())
	}
}

// The picker is the FIRST model choice only: any model history — active,
// preferred, downloaded, downloading — skips it (the owner-ruled full
// re-run is waired-agent#599), and the skip withdraws the
// pending-question claim so the daemon's fallback proceeds.
func TestModelPicker_SkipsWhenTheHostHasModelHistory(t *testing.T) {
	for name, mutate := range map[string]func(*catalogDetailResp){
		"active model": func(c *catalogDetailResp) { c.Families[1].Active = true },
		// An ANSWERED question, not merely a stored preference: the
		// daemon reports whether a person here gave it (waired-agent#627).
		"the question was answered here": func(c *catalogDetailResp) {
			c.PreferredModelID = "qwen3.5-9b"
			c.ModelQuestionAnswered = true
			c.Families[1].Preferred = true
		},
		"weights on disk": func(c *catalogDetailResp) { c.Families[0].Downloaded = true },
		"download in flight": func(c *catalogDetailResp) {
			c.Families[1].Downloading = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			cat := pickerCatalog()
			mutate(&cat)
			f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
			var out strings.Builder
			got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

			if got.none || got.picked != "" {
				t.Fatalf("outcome = %+v, want a silent skip", got)
			}
			if bodies := f.preferredBodies(); len(bodies) != 0 {
				t.Errorf("no preference may be written on a skip, got %v", bodies)
			}
			if claims := f.pendingClaims(); len(claims) != 1 || claims[0] {
				t.Errorf("the skip must withdraw the claim, got %v", claims)
			}
			if out.String() != "" {
				t.Errorf("a skipped picker prints nothing, got:\n%s", out.String())
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#627): a preference this host was HANDED
// is not an answer, so the picker still runs. The install flow's model
// step is an owner-ruled step (waired-ai/waired#1067, 2026-08-08), and on
// a real first install it silently disappeared because the setup path
// wrote preferred-model.json five minutes before the picker would have
// asked.
//
// This test inverts part of the one above it: "a stored preference" used
// to be listed there as a reason to skip, unconditionally.
func TestModelPicker_AHandedPreferenceIsNotAnAnswer(t *testing.T) {
	cat := pickerCatalog()
	// Exactly what the reconciler applying a control-plane instruction
	// leaves behind: a preference, and the row flagged from it — but no
	// answer given here.
	cat.PreferredModelID = "qwen3.5-9b"
	cat.ModelQuestionAnswered = false
	cat.Families[1].Preferred = true

	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("1\n"), &out, mine)

	if got.picked != "qwen3.5-2b" {
		t.Fatalf("outcome = %+v, want the picker to have run and taken row 1", got)
	}
	if !strings.Contains(out.String(), "Choose the AI model for this computer") {
		t.Errorf("the picker did not render:\n%s", out.String())
	}
}

// An older daemon does not report the new field, so it decodes false.
// That must not turn every configured host back into a first install:
// the weights-based signals still answer, exactly as they did before.
func TestModelPicker_OlderDaemonStillSkipsOnWeights(t *testing.T) {
	cat := pickerCatalog()
	cat.PreferredModelID = "qwen3.5-9b" // an old daemon sends this and nothing else
	cat.Families[1].Downloaded = true

	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
	var out strings.Builder
	got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

	if got.none || got.picked != "" {
		t.Fatalf("outcome = %+v, want a silent skip", got)
	}
	if out.String() != "" {
		t.Errorf("a skipped picker prints nothing, got:\n%s", out.String())
	}
}

// PRODUCT CONTRACT (waired-agent#607): the host-speed probe's own
// download is NOT model history. #496's probe pulls
// hostfit.HostCutoffProbeModelID through the same registry the catalog
// reports from, and that model is a real catalog entry rather than a
// private fixture, so counting its weights as a decision made the picker
// unreachable on the path it was designed for — deterministically, since
// step 6 waits for the very measurement that pull produces.
//
// The split is who chose it. Waired downloaded it to measure the host;
// a person may still PICK it (quality_tier 12, the smallest offered
// entry), and then it is history like any other model.
func TestModelPicker_ProbeModelIsNotModelHistory(t *testing.T) {
	probeCatalog := func(mutate func(*catalogDetailFamily)) catalogDetailResp {
		var c catalogDetailResp
		probe := catalogDetailFamily{ModelID: hostfit.HostCutoffProbeModelID, Fits: true}
		mutate(&probe)
		c.Families = []catalogDetailFamily{
			probe,
			{ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, RecommendedPick: true},
		}
		return c
	}

	t.Run("waired downloaded it, so the question still gets asked", func(t *testing.T) {
		for name, mutate := range map[string]func(*catalogDetailFamily){
			"weights on disk":    func(f *catalogDetailFamily) { f.Downloaded = true },
			"download in flight": func(f *catalogDetailFamily) { f.Downloading = true },
			"both":               func(f *catalogDetailFamily) { f.Downloaded, f.Downloading = true, true },
		} {
			t.Run(name, func(t *testing.T) {
				f := &pickerFakeDaemon{catalogs: []catalogDetailResp{probeCatalog(mutate)}}
				var out strings.Builder
				got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

				if !got.none {
					t.Fatalf("outcome = %+v, want the none answer — the picker never ran", got)
				}
				if !strings.Contains(out.String(), "Choose the AI model for this computer") {
					t.Errorf("the list was never printed:\n%s", out.String())
				}
			})
		}
	})

	t.Run("a person picked it, so it is history", func(t *testing.T) {
		for name, cat := range map[string]catalogDetailResp{
			"active": probeCatalog(func(f *catalogDetailFamily) { f.Active = true }),
			// "A person picked it" is the premise of this arm, and since
			// waired-agent#627 that is a claim the daemon makes explicitly
			// rather than one inferred from the preference existing.
			"preferred": func() catalogDetailResp {
				c := probeCatalog(func(f *catalogDetailFamily) { f.Preferred = true })
				c.ModelQuestionAnswered = true
				return c
			}(),
			"stored preference": func() catalogDetailResp {
				c := probeCatalog(func(*catalogDetailFamily) {})
				c.PreferredModelID = hostfit.HostCutoffProbeModelID
				c.ModelQuestionAnswered = true
				return c
			}(),
			"picked AND still marked downloaded": probeCatalog(func(f *catalogDetailFamily) {
				f.Active, f.Downloaded = true, true
			}),
		} {
			t.Run(name, func(t *testing.T) {
				f := &pickerFakeDaemon{catalogs: []catalogDetailResp{cat}}
				var out strings.Builder
				got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

				if got.none || got.picked != "" || out.String() != "" {
					t.Fatalf("outcome = %+v out=%q, want a silent skip", got, out.String())
				}
			})
		}
	})

	// The exclusion is for the probe model alone: any OTHER model's
	// weights still mean this host has decided something. Guards against
	// a fix that drops the Downloaded/Downloading arms altogether.
	t.Run("another model's weights are still history", func(t *testing.T) {
		var c catalogDetailResp
		c.Families = []catalogDetailFamily{
			{ModelID: hostfit.HostCutoffProbeModelID, Fits: true, Downloaded: true},
			{ModelID: "qwen3.5-9b", Fits: true, RecommendedPick: true, Downloaded: true},
		}
		f := &pickerFakeDaemon{catalogs: []catalogDetailResp{c}}
		var out strings.Builder
		got := runInitModelPicker(f.server(t).URL, false, "", linesOf("0\n"), &out, mine)

		if got.none || got.picked != "" || out.String() != "" {
			t.Fatalf("outcome = %+v out=%q, want a silent skip", got, out.String())
		}
	})
}

// The engine ask's precedence, applied here too: --non-interactive keeps
// the daemon's auto-selection, an explicit --inference-bundled-model-id
// pin IS the answer, a browser takeover owns the terminal, and an older
// daemon with no catalog fails open — all silent, all withdrawing the
// claim.
func TestModelPicker_SkipPrecedence(t *testing.T) {
	cases := map[string]struct {
		nonInteractive bool
		pin            string
		noCat          bool
		stillMine      func() bool
	}{
		"non-interactive":  {nonInteractive: true, stillMine: mine},
		"pinned model":     {pin: "qwen3.5-9b", stillMine: mine},
		"browser takeover": {stillMine: func() bool { return false }},
		"no catalog":       {noCat: true, stillMine: mine},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}, noCat: tc.noCat}
			var out strings.Builder
			got := runInitModelPicker(f.server(t).URL, tc.nonInteractive, tc.pin, linesOf("0\n"), &out, tc.stillMine)

			if got.none || got.picked != "" || out.String() != "" {
				t.Fatalf("outcome = %+v out=%q, want a silent skip", got, out.String())
			}
			if claims := f.pendingClaims(); len(claims) != 1 || claims[0] {
				t.Errorf("the skip must withdraw the claim, got %v", claims)
			}
		})
	}
}

// An answer is its own claim withdrawal on the daemon side, so answered
// runs must NOT also post pending=false — that would race the daemon's
// own bookkeeping for no reason.
func TestModelPicker_AnAnswerDoesNotAlsoWithdrawTheClaim(t *testing.T) {
	f := &pickerFakeDaemon{catalogs: []catalogDetailResp{pickerCatalog()}}
	var out strings.Builder
	_ = runInitModelPicker(f.server(t).URL, false, "", eofLineReader(), &out, mine)

	if claims := f.pendingClaims(); len(claims) != 0 {
		t.Errorf("an answered picker posts no pending claims, got %v", claims)
	}
}

// PRODUCT CONTRACT (waired-agent#632; wording approved 2026-08-11): a
// row that fits but spills its context cache says so IN THE PICKER, in
// the same words `models ls --detail` uses.
//
// The picker is where the choice is actually made. A cost that only
// appears on a surface the operator has to go looking for is one they
// learn about after the download.
func TestModelPickerRow_NamesTheContextCacheThatSpills(t *testing.T) {
	host := catalogDetailHost{RAMTotalGB: 32, VRAMTotalMB: 8188, GPUBudgetMB: 8188}
	spills := catalogDetailFamily{
		ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, RecommendedPick: true,
		Fit: &catalogDetailFit{Runnable: true, RequiredWindowResidentMB: 10719},
	}

	got := modelPickerRow(host, spills)
	const want = "Qwen3.5 9B — recommended for this computer · 2.5 GB of context cache in system RAM"
	if got != want {
		t.Errorf("row = %q, want %q", got, want)
	}

	// Byte-identical to the other surface's clause: two spellings of one
	// fact is the defect #649 fixed on the recommendation, and this is
	// the same pair of surfaces.
	fitCol := catalogFitColumn(host, spills)
	const clause = " · 2.5 GB of context cache in system RAM"
	if !strings.HasSuffix(fitCol, clause) {
		t.Fatalf("models ls --detail says %q; the picker must reuse that clause verbatim", fitCol)
	}
	if !strings.HasSuffix(got, clause) {
		t.Errorf("picker row %q does not end with the shared clause %q", got, clause)
	}
}

// The three silent cases. Each is "nothing to say" rather than "nothing
// spills", so each prints nothing rather than a measured-looking 0 GB.
func TestModelPickerRow_SaysNothingWhenThereIsNoFigure(t *testing.T) {
	fits := catalogDetailFamily{
		ModelID: "qwen3.5-2b", DisplayName: "Qwen3.5 2B", Fits: true,
		Fit: &catalogDetailFit{Runnable: true, RequiredWindowResidentMB: 4000},
	}
	for _, tc := range []struct {
		name string
		host catalogDetailHost
		fam  catalogDetailFamily
	}{
		{"it fits on the card", catalogDetailHost{GPUBudgetMB: 8188}, fits},
		{"no card, so no budget", catalogDetailHost{RAMTotalGB: 32}, fits},
		{"a daemon too old to report a budget", catalogDetailHost{RAMTotalGB: 32}, fits},
		{"no fit projection at all", catalogDetailHost{GPUBudgetMB: 8188},
			catalogDetailFamily{ModelID: "qwen3.5-2b", DisplayName: "Qwen3.5 2B", Fits: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelPickerRow(tc.host, tc.fam); got != "Qwen3.5 2B" {
				t.Errorf("row = %q, want the bare name with no cache clause", got)
			}
		})
	}

	// A row that does not fit at all keeps its deficit prose and gains no
	// second clause: "does not fit" and "spills part of the cache" are
	// different verdicts, and printing both reads as a contradiction.
	unfit := catalogDetailFamily{
		ModelID: "qwen3.5-27b", DisplayName: "Qwen3.5 27B", Fits: false,
		DeficitLabel: "needs 24 GB RAM (have 16 GB)",
		Fit:          &catalogDetailFit{RequiredWindowResidentMB: 30000},
	}
	got := modelPickerRow(catalogDetailHost{GPUBudgetMB: 8188}, unfit)
	if got != "Qwen3.5 27B — needs 24 GB RAM (have 16 GB)" {
		t.Errorf("unfit row = %q, want only the deficit", got)
	}
}
