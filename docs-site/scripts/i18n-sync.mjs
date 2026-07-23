#!/usr/bin/env node
// i18n-sync — keep the Japanese mirror honest.
//
// The problem this solves: `ja/` is a 1:1 mirror of the English tree, and
// nothing stopped an English page from being edited while its Japanese
// counterpart silently kept describing the old behaviour. Starlight's
// fallback only covers a *missing* page; a stale one looks perfectly fine.
//
// The mechanism: every `ja/` page carries `sourceHash` in its frontmatter —
// a digest of the English page it was translated from. CI recomputes that
// digest; if the English page moved on, the check fails and names the file.
// Accepting a change (after translating it, or after deciding an
// English-only edit needs no translation) is one command.
//
//   node scripts/i18n-sync.mjs --check              # CI gate
//   node scripts/i18n-sync.mjs --report             # human-readable table
//   node scripts/i18n-sync.mjs --accept <path...>   # refresh those hashes
//   node scripts/i18n-sync.mjs --accept --all       # refresh everything
//
// Paths for --accept may be given as either side of the pair (English or
// Japanese, absolute or repo-relative) — the script resolves to the pair.

import { createHash } from 'node:crypto';
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

// digest is the identity of an English page as far as translation is
// concerned. Line endings are normalised so a CRLF checkout does not
// invalidate the whole tree; nothing else is stripped, because a change to
// a code sample or a table cell matters to the translator exactly as much
// as a change to a sentence.
function digest(absPath) {
	const text = fs.readFileSync(absPath, 'utf8').replace(/\r\n/g, '\n');
	return createHash('sha256').update(text).digest('hex').slice(0, 16);
}

// splitFrontmatter returns [frontmatterBody, rest] for a page, or null when
// the file has no frontmatter block (which is itself an error for a page).
function splitFrontmatter(text) {
	const m = /^---\n([\s\S]*?)\n---\n?/.exec(text);
	if (!m) return null;
	return [m[1], text.slice(m[0].length), m[0]];
}

function readSourceHash(absPath) {
	const parts = splitFrontmatter(fs.readFileSync(absPath, 'utf8'));
	if (!parts) return null;
	const m = /^sourceHash:\s*(\S+)\s*$/m.exec(parts[0]);
	return m ? m[1] : null;
}

// writeSourceHash sets (or replaces) the sourceHash line in a Japanese
// page's frontmatter, appending it at the end of the block so it reads as
// bookkeeping rather than as content.
function writeSourceHash(absPath, hash) {
	const text = fs.readFileSync(absPath, 'utf8');
	const parts = splitFrontmatter(text);
	if (!parts) throw new Error(`no frontmatter: ${absPath}`);
	const [fm, rest] = parts;
	const line = `sourceHash: ${hash}`;
	const next = /^sourceHash:\s*\S+\s*$/m.test(fm)
		? fm.replace(/^sourceHash:\s*\S+\s*$/m, line)
		: `${fm.replace(/\s*$/, '')}\n${line}`;
	fs.writeFileSync(absPath, `---\n${next}\n---\n${rest}`);
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

// classify is the single place that decides what state a pair is in, so
// --check, --report and --accept can never disagree about it.
function classify(pair) {
	const want = digest(pair.en);
	if (!fs.existsSync(pair.ja)) return { state: 'missing', want };
	const have = readSourceHash(pair.ja);
	if (!have) return { state: 'unmarked', want };
	if (have !== want) return { state: 'stale', want, have };
	return { state: 'ok', want };
}

// resolvePair maps any user-supplied path onto the pair it belongs to.
function resolvePair(input, all) {
	const abs = path.resolve(process.cwd(), input);
	const hit = all.find((p) => p.en === abs || p.ja === abs);
	if (hit) return hit;
	// Also accept a bare slug ("guides/claude-code").
	const bare = all.find((p) => p.rel.replace(PAGE_RE, '') === input.replace(PAGE_RE, ''));
	if (bare) return bare;
	return null;
}

// ------------------------------------------------------------------ modes

function runCheck({ quiet = false } = {}) {
	const all = pairs();
	const bad = { missing: [], unmarked: [], stale: [] };
	for (const pair of all) {
		const res = classify(pair);
		if (res.state !== 'ok') bad[res.state].push(pair.rel);
	}
	const total = bad.missing.length + bad.unmarked.length + bad.stale.length;
	if (total === 0) {
		if (!quiet) console.log(`i18n-sync: ${all.length} page pairs, all in sync.`);
		return 0;
	}

	console.error(`\ni18n-sync: ${total} of ${all.length} Japanese pages need attention.\n`);
	if (bad.missing.length) {
		console.error('  Missing — the English page has no Japanese counterpart:');
		for (const rel of bad.missing) console.error(`    src/content/docs/ja/${rel}`);
		console.error('');
	}
	if (bad.unmarked.length) {
		console.error('  Unmarked — the Japanese page has no sourceHash frontmatter:');
		for (const rel of bad.unmarked) console.error(`    src/content/docs/ja/${rel}`);
		console.error('');
	}
	if (bad.stale.length) {
		console.error('  Stale — the English page changed after this translation:');
		for (const rel of bad.stale) console.error(`    src/content/docs/ja/${rel}`);
		console.error('');
	}
	console.error('  To resolve: update the Japanese page, then record it as current:');
	console.error('    npm run i18n:accept -- <path>       (or --all)');
	console.error('  An English-only edit that needs no translation is accepted the');
	console.error('  same way — the hash records "this pair was looked at".\n');
	return 1;
}

function runReport() {
	const all = pairs();
	const rows = all.map((p) => ({ rel: p.rel, ...classify(p) }));
	const width = Math.max(...rows.map((r) => r.rel.length));
	for (const r of rows) {
		const mark = { ok: 'ok      ', missing: 'MISSING ', unmarked: 'UNMARKED', stale: 'STALE   ' }[r.state];
		console.log(`${mark}  ${r.rel.padEnd(width)}  ${r.have ?? ''}${r.have ? ' -> ' : ''}${r.want}`);
	}
	const n = rows.filter((r) => r.state !== 'ok').length;
	console.log(`\n${rows.length} pairs, ${n} out of sync.`);
	return 0;
}

function runAccept(args) {
	const all = pairs();
	const targets = args.includes('--all')
		? all
		: args.filter((a) => !a.startsWith('--')).map((a) => {
				const hit = resolvePair(a, all);
				if (!hit) {
					console.error(`i18n-sync: not a documented page pair: ${a}`);
					process.exit(2);
				}
				return hit;
			});
	if (targets.length === 0) {
		console.error('i18n-sync: --accept needs one or more paths, or --all.');
		return 2;
	}
	let changed = 0;
	for (const pair of targets) {
		const res = classify(pair);
		if (res.state === 'missing') {
			console.error(`  skip (no Japanese page): ${pair.rel}`);
			continue;
		}
		if (res.state === 'ok') continue;
		writeSourceHash(pair.ja, res.want);
		console.log(`  accepted: ja/${pair.rel}  -> ${res.want}`);
		changed++;
	}
	console.log(`i18n-sync: ${changed} page${changed === 1 ? '' : 's'} recorded as current.`);
	return 0;
}

// ------------------------------------------------------------------- main

const argv = process.argv.slice(2);
const mode = argv.find((a) => ['--check', '--report', '--accept'].includes(a)) ?? '--check';
let code = 0;
if (mode === '--accept') code = runAccept(argv);
else if (mode === '--report') code = runReport();
else code = runCheck();
process.exit(code);
