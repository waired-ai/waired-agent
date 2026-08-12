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
// The second question, and why it needed its own answer: `sourceHash` is
// derived from the ENGLISH page alone, so it can only ever say "ja has
// acknowledged the current en" — never "ja still contains its own
// content". When two PRs edit the same English page, the ja pages
// conflict on the `sourceHash` LINE while their bodies merge cleanly, and
// the documented resolution (re-derive the hash) makes the check green
// again whether or not a paragraph survived the merge. The site builds
// fine with a Japanese paragraph missing. So a pair that claims to be
// current is also compared structurally — see `structure` (#678).
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

// structure counts the two things a translation must NOT change: how
// many headings the page has, and how many fenced code blocks.
//
// sourceHash answers "has ja acknowledged the current en", which is not
// the same question as "does ja still contain its own content". Nothing
// downstream asked the second one: the hash is derived from the English
// page alone, and the site builds perfectly well with a Japanese
// paragraph missing. Translation changes sentence counts freely, but a
// whole paragraph going missing almost always takes a heading or a code
// block with it, so counting those two catches the loss without
// pretending to compare prose.
//
// Headings inside fences are not headings — a shell comment starting
// with `#` is the common case, and counting it would make the check
// disagree with itself the moment a code sample changed.
function structure(absPath) {
	const text = fs.readFileSync(absPath, 'utf8').replace(/\r\n/g, '\n');
	const parts = splitFrontmatter(text);
	const body = parts ? parts[1] : text;
	let headings = 0;
	let fences = 0;
	let inFence = false;
	for (const line of body.split('\n')) {
		if (/^\s{0,3}(```|~~~)/.test(line)) {
			inFence = !inFence;
			if (inFence) fences++;
			continue;
		}
		if (!inFence && /^#{1,6}\s/.test(line)) headings++;
	}
	return { headings, fences };
}

// classify is the single place that decides what state a pair is in, so
// --check, --report and --accept can never disagree about it.
function classify(pair) {
	const want = digest(pair.en);
	if (!fs.existsSync(pair.ja)) return { state: 'missing', want };
	const have = readSourceHash(pair.ja);
	if (!have) return { state: 'unmarked', want };
	if (have !== want) return { state: 'stale', want, have };
	// Structure is only compared once the hashes agree, and that ordering
	// is the whole reason this is usable. While a pair is `stale` the
	// English page has moved and the Japanese one has not caught up yet —
	// a heading added on one side and not yet the other is the NORMAL
	// state of work in progress, and failing on it would fire on every
	// honest translation. A pair claiming to be current has no such
	// excuse.
	const en = structure(pair.en);
	const ja = structure(pair.ja);
	if (en.headings !== ja.headings || en.fences !== ja.fences) {
		return { state: 'drifted', want, en, ja };
	}
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
	const bad = { missing: [], unmarked: [], stale: [], drifted: [] };
	for (const pair of all) {
		const res = classify(pair);
		if (res.state === 'drifted') {
			bad.drifted.push(
				`${pair.rel}  (en: ${res.en.headings} headings, ${res.en.fences} code blocks; ` +
					`ja: ${res.ja.headings} headings, ${res.ja.fences} code blocks)`,
			);
		} else if (res.state !== 'ok') {
			bad[res.state].push(pair.rel);
		}
	}
	const total = bad.missing.length + bad.unmarked.length + bad.stale.length + bad.drifted.length;
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
	if (bad.drifted.length) {
		// Listed last and worded differently on purpose: the three above
		// are "translate this", which --accept finishes. This one is
		// "content is missing", which --accept cannot fix and must not
		// paper over.
		console.error('  Drifted — the Japanese page claims to be current, but its shape');
		console.error('  no longer matches the English page. A heading or a code block is');
		console.error('  missing on one side; the usual cause is a paragraph lost while');
		console.error('  resolving a sourceHash conflict.');
		for (const line of bad.drifted) console.error(`    src/content/docs/ja/${line}`);
		console.error('');
		console.error('  Fix this one by restoring the missing content, NOT with --accept:');
		console.error('  the hash already matches, so accepting would record the loss as');
		console.error('  intentional. Compare the two pages side by side.\n');
	}
	console.error('  To resolve: update the Japanese page, then record it as current:');
	console.error('    npm run i18n:accept -- <path>       (or --all)');
	console.error('  Keep the pinned terminology in docs-site/TRANSLATION.md — never');
	console.error('  re-derive those term choices while retranslating.');
	console.error('  An English-only edit that needs no translation is accepted the');
	console.error('  same way — the hash records "this pair was looked at".\n');
	return 1;
}

function runReport() {
	const all = pairs();
	const rows = all.map((p) => ({ rel: p.rel, ...classify(p) }));
	const width = Math.max(...rows.map((r) => r.rel.length));
	for (const r of rows) {
		const mark = {
			ok: 'ok      ',
			missing: 'MISSING ',
			unmarked: 'UNMARKED',
			stale: 'STALE   ',
			drifted: 'DRIFTED ',
		}[r.state];
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
	let refused = 0;
	for (const pair of targets) {
		const res = classify(pair);
		if (res.state === 'missing') {
			console.error(`  skip (no Japanese page): ${pair.rel}`);
			continue;
		}
		if (res.state === 'ok') continue;
		if (res.state === 'drifted') {
			// Refused, not silently skipped. The hash already matches, so
			// writing it would be a no-op that PRINTS like an acceptance —
			// and this is the one state where "I looked at it and it is
			// fine" is exactly the wrong record to leave behind.
			console.error(
				`  REFUSED (content is missing, not stale): ja/${pair.rel}\n` +
					`    en has ${res.en.headings} headings / ${res.en.fences} code blocks, ` +
					`ja has ${res.ja.headings} / ${res.ja.fences}.\n` +
					'    Restore the missing content; there is no hash to refresh here.',
			);
			refused++;
			continue;
		}
		writeSourceHash(pair.ja, res.want);
		console.log(`  accepted: ja/${pair.rel}  -> ${res.want}`);
		changed++;
	}
	console.log(`i18n-sync: ${changed} page${changed === 1 ? '' : 's'} recorded as current.`);
	if (refused > 0) {
		console.error(`i18n-sync: ${refused} page${refused === 1 ? '' : 's'} refused — see above.`);
		return 1;
	}
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
