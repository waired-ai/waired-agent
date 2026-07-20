// Build-time loader for the model-catalog table. Reads the bundled
// manifests (proto/catalog/bundled/*.json — the same files the agent
// embeds) at `astro build` time so the docs table can never drift from
// the shipped catalog. Pure Node fs; runs server-side during the build.
//
// Resolved from the working directory rather than import.meta.url: Vite
// bundles this module into dist/.prerender/chunks, so import.meta.url
// would point at the relocated chunk. `astro build` always runs with cwd
// = docs-site/, so the manifests sit one level up; a repo-root fallback
// keeps it working if invoked from elsewhere.
//
// The legacy internal/catalog/bundled candidates are kept because #101
// moved the catalog data layer to proto/catalog and this loader was not
// updated with it: the deploy workflow is path-filtered to docs-site/**,
// so the break stayed latent until the next docs change. Keeping both
// makes the loader survive the move in either direction.
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

function resolveBundledDir(): string {
	const candidates = [
		join(process.cwd(), '..', 'proto', 'catalog', 'bundled'), // cwd = docs-site
		join(process.cwd(), 'proto', 'catalog', 'bundled'), // cwd = repo root
		join(process.cwd(), '..', 'internal', 'catalog', 'bundled'), // pre-#101
		join(process.cwd(), 'internal', 'catalog', 'bundled'), // pre-#101
	];
	for (const c of candidates) {
		if (existsSync(c)) return c;
	}
	// Fall back to the docs-site-relative path so the thrown ENOENT names
	// the expected location.
	return candidates[0];
}

const BUNDLED_DIR = resolveBundledDir();

const DEFAULT_ALIAS = 'waired/default';

interface Variant {
	runtime_support?: string[];
	min_ram_gb?: number;
	min_vram_mb?: number;
	quality_tier?: number;
	param_count?: number;
	active_params?: number;
}

interface Manifest {
	model_id: string;
	display_name?: string;
	model_aliases?: string[];
	variants?: Variant[];
}

export interface CatalogRow {
	modelId: string;
	displayName: string;
	/** e.g. "7.6B" or "30B (3.3B active)". */
	paramsLabel: string;
	/** Highest variant quality_tier in the family, or null. */
	qualityTier: number | null;
	/** Smallest min_ram_gb among ollama variants, or null (vLLM-only). */
	ollamaRamGB: number | null;
	/** Smallest min_vram_mb (→GB, rounded up) among vLLM variants, or null. */
	vllmVramGB: number | null;
	isDefault: boolean;
	/** Total param count — used only for sorting. */
	paramCount: number;
	/**
	 * True for Mixture-of-Experts families (a variant reports an active
	 * parameter count below the total). Drives the Dense/MoE table split.
	 */
	isMoE: boolean;
}

function humanizeParams(n: number): string {
	const B = 1_000_000_000;
	const M = 1_000_000;
	if (n >= B) {
		const v = n / B;
		return v >= 100 || Number.isInteger(v) ? `${Math.round(v)}B` : `${v.toFixed(1)}B`;
	}
	if (n >= M) return `${Math.round(n / M)}M`;
	return `${n}`;
}

/** loadCatalog returns one row per bundled manifest, smallest first. */
export function loadCatalog(): CatalogRow[] {
	const files = readdirSync(BUNDLED_DIR)
		.filter((f) => f.endsWith('.json'))
		.sort();

	const rows: CatalogRow[] = files.map((file) => {
		const m: Manifest = JSON.parse(readFileSync(join(BUNDLED_DIR, file), 'utf8'));
		const variants = m.variants ?? [];

		const ollamaRam = variants
			.filter((v) => (v.runtime_support ?? []).includes('ollama') && (v.min_ram_gb ?? 0) > 0)
			.map((v) => v.min_ram_gb as number);
		const vllmVram = variants
			.filter((v) => (v.runtime_support ?? []).includes('vllm') && (v.min_vram_mb ?? 0) > 0)
			.map((v) => v.min_vram_mb as number);
		const tiers = variants.map((v) => v.quality_tier ?? 0).filter((t) => t > 0);

		const total = variants.find((v) => (v.param_count ?? 0) > 0)?.param_count ?? 0;
		const active = variants.find((v) => (v.active_params ?? 0) > 0)?.active_params ?? 0;
		const isMoE = total > 0 && active > 0 && active < total;
		let paramsLabel = total > 0 ? humanizeParams(total) : '—';
		if (isMoE) {
			paramsLabel += ` (${humanizeParams(active)} active)`;
		}

		return {
			modelId: m.model_id,
			displayName: m.display_name ?? m.model_id,
			paramsLabel,
			qualityTier: tiers.length ? Math.max(...tiers) : null,
			ollamaRamGB: ollamaRam.length ? Math.min(...ollamaRam) : null,
			vllmVramGB: vllmVram.length ? Math.ceil(Math.min(...vllmVram) / 1024) : null,
			isDefault: (m.model_aliases ?? []).includes(DEFAULT_ALIAS),
			paramCount: total,
			isMoE,
		};
	});

	rows.sort((a, b) => a.paramCount - b.paramCount || a.modelId.localeCompare(b.modelId));
	return rows;
}
