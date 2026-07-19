package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// docs regenerates the machine-generated model table embedded in the
// repo docs page docs/reference/models.md (until the #184 split this
// lived in the monorepo's dev-docs-site, which now carries a pointer
// instead). The page answers "which models ship?" — a question the
// prose pages deliberately leave to the catalog — and it must never
// drift from proto/catalog/bundled/*.json. So instead of a
// hand-maintained table (which the weekly catalog-radar bot, #413, would rot),
// the table is rendered from catalog.BundledManifests() and the rendered region
// lives between two HTML-comment markers in the page. `--check` re-renders and
// diffs without writing, mirroring `catalog-tool validate --all`, so CI (and the
// catalog-radar draft PRs that touch bundled/*.json) keep the page fresh.
const (
	docsDefaultFile = "docs/reference/models.md"
	docsBeginMarker = "<!-- BEGIN GENERATED: catalog-tool docs -->"
	docsEndMarker   = "<!-- END GENERATED: catalog-tool docs -->"
)

// fixedAliases are the product-fixed catalog aliases the coding-agent
// integrations present. waired/default and waired/coding are dynamic since
// #632 — the router resolves them at request time to the host's current
// coding default (preferred > active > bundled), so no manifest owns them;
// only the size aliases (waired/small) remain static ModelAliases entries.
// Listed here so the generated alias table is deterministic and ordered.
var fixedAliases = []string{"waired/default", "waired/coding", "waired/small"}

// dynamicAliasNote is the rendered target for aliases no manifest owns
// because the router resolves them dynamically (#632).
var dynamicAliasNote = map[string]string{
	"waired/default": "動的: このホストの既定コーディングモデル（preferred > active > bundled）",
	"waired/coding":  "動的: waired/default と同じ解決",
}

func init() {
	subcommands["docs"] = subcommand{run: runDocs, summary: "regenerate the bundled-model table in the dev docs (reference/models.md)"}
}

func runDocs(args []string) error {
	fs := flag.NewFlagSet("docs", flag.ContinueOnError)
	file := fs.String("file", docsDefaultFile, "path to the model-catalog page to update")
	check := fs.Bool("check", false, "verify the page is up to date; exit non-zero (no write) if it drifted")
	if err := fs.Parse(args); err != nil {
		return err
	}

	manifests, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("docs: load bundled catalog: %w", err)
	}
	block := renderCatalogBlock(manifests)

	existing, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("docs: read %s: %w", *file, err)
	}
	updated, err := spliceGeneratedBlock(existing, block)
	if err != nil {
		return fmt.Errorf("docs: %s: %w", *file, err)
	}

	if *check {
		if !bytes.Equal(existing, updated) {
			return fmt.Errorf("docs: %s is stale — regenerate with `make catalog-docs` (or `catalog-tool docs`) and commit the result", *file)
		}
		fmt.Printf("ok: %s up to date (%d bundled manifests)\n", *file, len(manifests))
		return nil
	}

	if bytes.Equal(existing, updated) {
		fmt.Printf("ok: %s already up to date (%d bundled manifests)\n", *file, len(manifests))
		return nil
	}
	if err := os.WriteFile(*file, updated, 0o644); err != nil {
		return fmt.Errorf("docs: write %s: %w", *file, err)
	}
	fmt.Printf("updated %s (%d bundled manifests)\n", *file, len(manifests))
	return nil
}

// spliceGeneratedBlock replaces the region between the begin/end markers with
// block, preserving everything outside the markers (frontmatter, prose,
// cross-links). It is idempotent: re-splicing an already-current document
// yields byte-identical output.
func spliceGeneratedBlock(doc []byte, block string) ([]byte, error) {
	s := string(doc)
	bi := strings.Index(s, docsBeginMarker)
	if bi < 0 {
		return nil, fmt.Errorf("begin marker %q not found", docsBeginMarker)
	}
	ei := strings.Index(s, docsEndMarker)
	if ei < 0 {
		return nil, fmt.Errorf("end marker %q not found", docsEndMarker)
	}
	if ei < bi {
		return nil, fmt.Errorf("end marker precedes begin marker")
	}
	var b strings.Builder
	b.WriteString(s[:bi+len(docsBeginMarker)])
	b.WriteString("\n\n")
	b.WriteString(block)
	b.WriteString("\n")
	b.WriteString(s[ei:])
	return []byte(b.String()), nil
}

// engineSections drives the top-level split of both catalog tables. The runtime
// a host's OS+GPU forces (engine_picker.go) is the hard fork a reader starts
// from, so it is the outer grouping; Dense-then-MoE is the inner grouping within
// each engine. A family that ships builds for both engines appears under both —
// intentional, so a reader only scans the section that matches their hardware.
var engineSections = []struct {
	id   string // runtime_support token: catalog.RuntimeOllama | catalog.RuntimeVLLM
	head string
}{
	{catalog.RuntimeOllama, "Ollama 経路（Mac / Windows / CPU / 内蔵・低VRAM GPU）"},
	{catalog.RuntimeVLLM, "vLLM 経路（NVIDIA / AMD GPU サーバ）"},
}

// renderCatalogBlock builds the markdown body (no surrounding markers, no
// trailing newline) for the bundled catalog: a fixed-alias map, then a per-family
// overview and the full per-variant numeric table — each split by engine
// (Ollama / vLLM) and architecture (Dense → MoE).
func renderCatalogBlock(manifests []catalog.Manifest) string {
	var b strings.Builder

	variantCount := 0
	for _, m := range manifests {
		variantCount += len(m.Variants)
	}

	b.WriteString("> この節は `proto/catalog/bundled/*.json` から `catalog-tool docs` が機械生成する。")
	b.WriteString("**手で編集しない** — モデルを追加・更新したら `make catalog-docs`（または `catalog-tool docs`）で再生成してコミットする。")
	b.WriteString("catalog-radar（#413）の自動更新も同じ経路を使う。空欄は `—`。\n\n")
	fmt.Fprintf(&b, "bundled 済み: **%d ファミリ / %d バリアント**。\n\n", len(manifests), variantCount)
	b.WriteString("ファミリ概要・全バリアント表は **エンジン（Ollama / vLLM）→ アーキテクチャ（Dense → MoE）** で分割する。")
	b.WriteString("エンジンはバリアント単位（`runtime_support`）なので、両エンジン向けにビルドを持つファミリは両節に再掲される。")
	b.WriteString("Dense=全パラメータが毎トークン計算（計算 / VRAM 余裕がある環境向き）、")
	b.WriteString("MoE=総サイズ大だがアクティブ少（メモリリッチな Unified メモリ機向き・デコード高速）。\n\n")

	// --- Fixed aliases -----------------------------------------------------
	b.WriteString("### 固定エイリアス\n\n")
	b.WriteString("コーディングエージェント連携が提示する 3 つの固定別名と、それが解決する bundled モデル。\n\n")
	b.WriteString("| エイリアス | 解決先 model_id | 表示名 |\n")
	b.WriteString("| --- | --- | --- |\n")
	for _, alias := range fixedAliases {
		target := "（未割当）"
		display := ""
		if note, dynamic := dynamicAliasNote[alias]; dynamic {
			target = note
		} else if m, ok := catalog.LookupByAlias(alias, manifests); ok {
			target = "`" + m.ModelID + "`"
			display = esc(m.DisplayName)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", alias, target, display)
	}
	b.WriteString("\n")

	// --- Family overview (engine → Dense/MoE) ------------------------------
	b.WriteString("### ファミリ概要\n\n")
	for _, es := range engineSections {
		fmt.Fprintf(&b, "#### %s\n\n", es.head)
		b.WriteString("**Dense**\n\n")
		writeFamilyTable(&b, filterFamilies(manifests, es.id, false))
		b.WriteString("**MoE（総 / 活性）**\n\n")
		writeFamilyTable(&b, filterFamilies(manifests, es.id, true))
	}

	// --- Full per-variant table (engine → Dense/MoE) ----------------------
	b.WriteString("### 全バリアント（数値）\n\n")
	b.WriteString("vendor_support の状態略号: `S`=stable / `E`=experimental / `C`=community / `×`=unsupported。")
	b.WriteString("weight GB は概算（`estimated_weight_gb`）、min VRAM は vLLM 経路、min RAM は ollama 経路の下限。")
	b.WriteString("数値の導出根拠は dev-docs の「推論層」と `internal/catalog/scoring/` を参照。\n\n")
	for _, es := range engineSections {
		fmt.Fprintf(&b, "#### %s\n\n", es.head)
		b.WriteString("**Dense**\n\n")
		writeVariantTable(&b, filterVariants(manifests, es.id, false))
		b.WriteString("**MoE（総 / 活性）**\n\n")
		writeVariantTable(&b, filterVariants(manifests, es.id, true))
	}

	b.WriteString("<!-- 自動生成セクションここまで。編集は `catalog-tool docs` 経由で。 -->")
	return strings.TrimRight(b.String(), "\n")
}

// variantIsMoE reports whether a variant is Mixture-of-Experts (an active-param
// count strictly below the total), matching paramsCell's annotation rule.
func variantIsMoE(v catalog.Variant) bool {
	return v.ActiveParams > 0 && v.ActiveParams < v.ParamCount
}

// familyIsMoE reports whether any variant of the family is MoE. Families are
// consistently Dense or MoE across their variants, so any-match suffices.
func familyIsMoE(m catalog.Manifest) bool {
	return slices.ContainsFunc(m.Variants, variantIsMoE)
}

func variantSupportsEngine(v catalog.Variant, engine string) bool {
	return slices.Contains(v.RuntimeSupport, engine)
}

func familySupportsEngine(m catalog.Manifest, engine string) bool {
	return slices.ContainsFunc(m.Variants, func(v catalog.Variant) bool {
		return variantSupportsEngine(v, engine)
	})
}

// filterFamilies returns the families that ship a build for engine and fall in
// the requested architecture class, preserving input order (model_id sort).
func filterFamilies(ms []catalog.Manifest, engine string, moe bool) []catalog.Manifest {
	var out []catalog.Manifest
	for _, m := range ms {
		if familySupportsEngine(m, engine) && familyIsMoE(m) == moe {
			out = append(out, m)
		}
	}
	return out
}

// writeFamilyTable renders the family-overview table for one engine×arch bucket,
// or a "該当なし" note when the bucket is empty (keeps the section shape stable).
func writeFamilyTable(b *strings.Builder, fams []catalog.Manifest) {
	if len(fams) == 0 {
		b.WriteString("*(該当なし)*\n\n")
		return
	}
	b.WriteString("| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, m := range fams {
		params := "—"
		if len(m.Variants) > 0 {
			params = paramsCell(m.Variants[0])
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s | %s | %d |\n",
			m.ModelID,
			esc(m.DisplayName),
			wairedAliases(m.ModelAliases),
			withCommas(m.ContextLength),
			esc(joinOrDash(m.Capabilities, ", ")),
			params,
			orDash(m.Runtime.Preferred),
			len(m.Variants),
		)
	}
	b.WriteString("\n")
}

// variantRow pairs a variant with its owning family's model_id for the
// per-variant table.
type variantRow struct {
	modelID string
	v       catalog.Variant
}

// filterVariants returns the variants that support engine and fall in the
// requested architecture class, in (model_id, manifest) order so a family's
// variants stay adjacent inside each bucket.
func filterVariants(ms []catalog.Manifest, engine string, moe bool) []variantRow {
	var out []variantRow
	for _, m := range ms {
		for _, v := range m.Variants {
			if variantSupportsEngine(v, engine) && variantIsMoE(v) == moe {
				out = append(out, variantRow{m.ModelID, v})
			}
		}
	}
	return out
}

// writeVariantTable renders the full numeric per-variant table for one
// engine×arch bucket, or a "該当なし" note when empty.
func writeVariantTable(b *strings.Builder, rows []variantRow) {
	if len(rows) == 0 {
		b.WriteString("*(該当なし)*\n\n")
		return
	}
	b.WriteString("| model_id | variant | format | quant | runtime | 品質 | 量子 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/活性） | attn | KV B/tok | vendor_support | source | min engine |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		v := r.v
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s | %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.modelID,
			v.VariantID,
			orDash(v.Format),
			quantCell(v),
			orDash(joinOrDash(v.RuntimeSupport, "/")),
			v.QualityTier,
			v.QuantizationTier,
			weightCell(v.EstimatedWeightGB),
			intOrDash(v.MinRAMGB),
			intOrDash(v.MinVRAMMB),
			paramsCell(v),
			orDash(v.AttentionArch),
			intOrDash(v.KVBytesPerTokenFP16),
			esc(vendorSupportCompact(v.VendorSupport)),
			esc(sourceCell(v.Source)),
			orDash(v.MinEngineVersion),
		)
	}
	b.WriteString("\n")
}

// wairedAliases returns the space-separated waired/* aliases a manifest carries
// (the product-facing names), or "—" if none.
func wairedAliases(aliases []string) string {
	var got []string
	for _, a := range aliases {
		if strings.HasPrefix(a, "waired/") {
			got = append(got, "`"+a+"`")
		}
	}
	return joinOrDash(got, " ")
}

// paramsCell renders the parameter count, annotating MoE variants with their
// active-parameter count (e.g. "30.5B / A3.3B").
func paramsCell(v catalog.Variant) string {
	total := humanizeParams(v.ParamCount)
	if v.ActiveParams > 0 && v.ActiveParams < v.ParamCount {
		return total + " / A" + humanizeParams(v.ActiveParams)
	}
	return total
}

func quantCell(v catalog.Variant) string {
	if v.Quantization != "" {
		return v.Quantization
	}
	if v.DType != "" {
		return v.DType
	}
	return "—"
}

func weightCell(gb float64) string {
	if gb <= 0 {
		return "—"
	}
	return strconv.FormatFloat(gb, 'f', 1, 64)
}

func sourceCell(s catalog.VariantSource) string {
	switch s.Type {
	case catalog.SourceOllama:
		return "ollama:" + orDash(s.Tag)
	case catalog.SourceHuggingFace:
		return "hf:" + orDash(s.RepoID)
	case "":
		return "—"
	default:
		return s.Type
	}
}

// vendorSupportCompact renders the vendor×runtime matrix compactly, listing only
// the cells the manifest set. nil / empty == permissive.
func vendorSupportCompact(v *catalog.VendorSupportMatrix) string {
	if v == nil {
		return "（permissive）"
	}
	var parts []string
	add := func(vendor string, c catalog.VendorRuntimeSupport) {
		var cells []string
		addCell := func(rt, status string) {
			if status == "" {
				return
			}
			cells = append(cells, rt+"="+abbrevStatus(status))
		}
		addCell("ollama", c.Ollama)
		addCell("vllm", c.VLLM)
		addCell("llama_cpp", c.LlamaCPP)
		addCell("mlx", c.MLX)
		if len(cells) > 0 {
			parts = append(parts, vendor+":"+strings.Join(cells, ","))
		}
	}
	add("nv", v.Nvidia)
	add("amd", v.AMD)
	add("mac", v.Mac)
	if len(parts) == 0 {
		return "（permissive）"
	}
	return strings.Join(parts, " · ")
}

func abbrevStatus(s string) string {
	switch s {
	case catalog.VendorSupportStable:
		return "S"
	case catalog.VendorSupportExperimental:
		return "E"
	case catalog.VendorSupportCommunity:
		return "C"
	case catalog.VendorSupportUnsupported:
		return "×"
	default:
		return s
	}
}

// humanizeParams formats a parameter count as billions with one decimal,
// trimming a trailing ".0" (e.g. 7610000000 → "7.6B", 122000000000 → "122B").
func humanizeParams(n int64) string {
	if n <= 0 {
		return "—"
	}
	s := strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + "B"
}

func withCommas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func joinOrDash(xs []string, sep string) string {
	if len(xs) == 0 {
		return "—"
	}
	return strings.Join(xs, sep)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func intOrDash(n int) string {
	if n == 0 {
		return "—"
	}
	return withCommas(n)
}

// esc neutralises markdown table delimiters in free-text cells.
func esc(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
