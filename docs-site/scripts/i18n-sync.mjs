#!/usr/bin/env node
// i18n-sync — keep the Japanese mirror honest.
//
// The problem this solves: `ja/` is a 1:1 mirror of the English tree, and
// nothing stopped an English page from being edited while its Japanese
// counterpart silently kept describing the old behaviour. Starlight's
// fallback only covers a *missing* page; a stale one looks perfectly fine.
//
// Freshness is not checked here. It is a rule about a pull request —
// changing an English page means changing its Japanese one in the same PR —
// and it is enforced from the diff by scripts/ci/i18n-pair-guard.sh. This
// script asks the two questions that are about the TREE and cannot be
// answered from a diff:
//
//   * is there a Japanese page at all for every English one, and
//   * do the two sides of a pair still have the same shape.
//
// It used to answer the freshness question too, from a `sourceHash` line in
// each ja page's frontmatter — a digest of the English page. That line was a
// derived value stored in a versioned file, so two pull requests touching one
// English page always rewrote it to two different values and always
// conflicted, on that line and nothing else. At this repository's lane count
// that stopped being a per-collision cost and became a condition on landing
// (waired-agent#1215). The hash is gone; the question it answered is now the
// PR guard's.
//
//   node scripts/i18n-sync.mjs --check     # CI gate
//   node scripts/i18n-sync.mjs --report    # human-readable table

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const DOCS = path.join(ROOT, 'src', 'content', 'docs');
const JA = path.join(DOCS, 'ja');

// Pages that deliberately exist in English only. Keep this list short and
// justified — every entry is a page a Japanese reader will hit in English.
// `index` is not eligible: the landing page must exist in both.
const EN_ONLY = new Set([
	// (none today)
]);

const PAGE_RE = /\.(md|mdx)$/;

// ---------------------------------------------------------------- helpers

// walk returns every page path under dir, repo-relative to DOCS, skipping
// the `ja/` subtree so the English enumeration stays clean.
function walk(dir, acc = []) {
	for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
		const abs = path.join(dir, entry.name);
		if (entry.isDirectory()) {
			if (abs === JA) continue;
			walk(abs, acc);
		} else if (PAGE_RE.test(entry.name)) {
			acc.push(path.relative(DOCS, abs));
		}
	}
	return acc;
}

// splitFrontmatter returns [frontmatterBody, rest] for a page, or null when
// the file has no frontmatter block (which is itself an error for a page).
function splitFrontmatter(text) {
	const m = /^---\n([\s\S]*?)\n---\n?/.exec(text);
	if (!m) return null;
	return [m[1], text.slice(m[0].length), m[0]];
}

// pairs enumerates every (english, japanese) page pair the mirror requires.
function pairs() {
	return walk(DOCS)
		.filter((rel) => !EN_ONLY.has(rel.replace(PAGE_RE, '')))
		.sort()
		.map((rel) => ({
			rel,
			en: path.join(DOCS, rel),
			ja: path.join(JA, rel),
		}));
}

// structure counts the things a translation must NOT change: how many
// headings the page has, how many fenced code blocks, and how many of
// each MDX component (`<LinkCard`, `<Aside>`, `<Expected>`, …) it places.
//
// The site builds perfectly well with a Japanese paragraph missing, and
// nothing else asks whether ja still contains its own content. Translation
// changes sentence counts freely, but a whole paragraph going missing
// almost always takes a heading or a code block with it, so counting those
// two catches the loss without pretending to compare prose.
//
// Headings inside fences are not headings — a shell comment starting
// with `#` is the common case, and counting it would make the check
// disagree with itself the moment a code sample changed.
//
// Components are counted by name, from their opening tag, so a
// `<LinkCard>` dropped from one side of a pair is caught even when the
// page keeps every heading and code block. That is exactly how the
// OpenCode restore lost the OpenClaw card from two English pages while
// this check said `all in sync` (waired-agent#1010). Tags inside
// fences, inline code and `{/* … */}` comments are skipped for the same
// reason headings inside fences are: they are samples or notes, not the
// page's structure. Only capitalised tags count — lowercase ones are
// HTML, which the two sides legitimately use differently (`<a id>`
// anchors, `<kbd>`).
function structure(absPath) {
	const text = fs.readFileSync(absPath, 'utf8').replace(/\r\n/g, '\n');
	const parts = splitFrontmatter(text);
	const body = (parts ? parts[1] : text).replace(/\{\/\*[\s\S]*?\*\/\}/g, '');
	let headings = 0;
	let fences = 0;
	const components = {};
	let inFence = false;
	for (const line of body.split('\n')) {
		if (/^\s{0,3}(```|~~~)/.test(line)) {
			inFence = !inFence;
			if (inFence) fences++;
			continue;
		}
		if (inFence) continue;
		if (/^#{1,6}\s/.test(line)) headings++;
		for (const m of line.replace(/`[^`]*`/g, '').matchAll(/<([A-Z][A-Za-z0-9]*)\b/g)) {
			components[m[1]] = (components[m[1]] ?? 0) + 1;
		}
	}
	return { headings, fences, components };
}

// componentDiff lists the component names whose counts differ between
// the two sides, as `Name en/ja`, so the failure says which card or
// callout is missing rather than just that one is.
function componentDiff(en, ja) {
	const names = new Set([...Object.keys(en.components), ...Object.keys(ja.components)]);
	const out = [];
	for (const name of [...names].sort()) {
		const a = en.components[name] ?? 0;
		const b = ja.components[name] ?? 0;
		if (a !== b) out.push(`${name} ${a}/${b}`);
	}
	return out;
}

// classify is the single place that decides what state a pair is in, so
// --check and --report can never disagree about it.
//
// Every pair is compared, always. The comparison used to be gated on the
// two hashes already agreeing, because while a pair was `stale` the English
// page had moved and the Japanese one had not caught up yet — differing
// shape was the normal state of work in progress, and failing on it would
// have fired on every honest translation. A pull request cannot be in that
// state any more: it changes both sides of a pair or it does not land
// (scripts/ci/i18n-pair-guard.sh, waired-agent#1215).
function classify(pair) {
	if (!fs.existsSync(pair.ja)) return { state: 'missing' };
	const en = structure(pair.en);
	const ja = structure(pair.ja);
	const components = componentDiff(en, ja);
	if (en.headings !== ja.headings || en.fences !== ja.fences || components.length) {
		return { state: 'drifted', en, ja, components };
	}
	return { state: 'ok' };
}

// ------------------------------------------------------------------ modes

function runCheck() {
	const all = pairs();
	const bad = { missing: [], drifted: [] };
	for (const pair of all) {
		const res = classify(pair);
		if (res.state === 'drifted') {
			const comp = res.components.length ? `; components en/ja: ${res.components.join(', ')}` : '';
			bad.drifted.push(
				`${pair.rel}  (en: ${res.en.headings} headings, ${res.en.fences} code blocks; ` +
					`ja: ${res.ja.headings} headings, ${res.ja.fences} code blocks${comp})`,
			);
		} else if (res.state !== 'ok') {
			bad[res.state].push(pair.rel);
		}
	}
	const total = bad.missing.length + bad.drifted.length;
	if (total === 0) {
		console.log(`i18n-sync: ${all.length} page pairs, all in sync.`);
		return 0;
	}

	console.error(`\ni18n-sync: ${total} of ${all.length} Japanese pages need attention.\n`);
	if (bad.missing.length) {
		console.error('  Missing — the English page has no Japanese counterpart:');
		for (const rel of bad.missing) console.error(`    src/content/docs/ja/${rel}`);
		console.error('');
	}
	if (bad.drifted.length) {
		console.error('  Drifted — the two sides of this pair no longer have the same');
		console.error('  shape. A heading, a code block or a component is missing on one');
		console.error('  side; the usual cause is a paragraph lost while resolving a');
		console.error('  merge conflict in the Japanese page.');
		for (const line of bad.drifted) console.error(`    src/content/docs/ja/${line}`);
		console.error('');
	}
	console.error('  To resolve: restore the missing content, or write the Japanese');
	console.error('  page the English one now describes — in the SAME pull request');
	console.error('  that changed the English page. Keep the pinned terminology in');
	console.error('  docs-site/TRANSLATION.md — never re-derive those term choices');
	console.error('  while retranslating.\n');
	return 1;
}

function runReport() {
	const all = pairs();
	const rows = all.map((p) => ({ rel: p.rel, ...classify(p) }));
	const width = Math.max(...rows.map((r) => r.rel.length));
	for (const r of rows) {
		const mark = { ok: 'ok      ', missing: 'MISSING ', drifted: 'DRIFTED ' }[r.state];
		const detail =
			r.state === 'drifted'
				? `en ${r.en.headings}h/${r.en.fences}f, ja ${r.ja.headings}h/${r.ja.fences}f` +
					(r.components.length ? `, ${r.components.join(', ')}` : '')
				: '';
		console.log(`${mark}  ${r.rel.padEnd(width)}  ${detail}`);
	}
	const n = rows.filter((r) => r.state !== 'ok').length;
	console.log(`\n${rows.length} pairs, ${n} out of sync.`);
	return 0;
}

// ------------------------------------------------------------------- main

const argv = process.argv.slice(2);
const mode = argv.find((a) => ['--check', '--report'].includes(a)) ?? '--check';
let code = 0;
if (mode === '--report') code = runReport();
else code = runCheck();
process.exit(code);
