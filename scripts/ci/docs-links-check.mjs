#!/usr/bin/env node
// docs-links-check: every root-relative link and #anchor in the built docs
// site resolves to a page and an element that exist.
//
// Usage: node scripts/ci/docs-links-check.mjs docs-site/dist
//
// Why this exists: Astro fails the build only for a sidebar `slug` that does
// not resolve. A body link to a page that was renamed, or to a heading whose
// id changed when its text changed, builds green and 404s on the live site.
// The Japanese tree is the usual victim, because a Japanese heading gets a
// different auto id from its English twin, so ja pages carry explicit
// <a id="..."> anchors and this script is what checks they are still there.
//
// Checks only what the build produced. External links (http://, https://,
// mailto:) are not fetched. Query strings are ignored. A link to a redirect
// page counts as resolved, because Astro writes one HTML file per redirect.

import { readdirSync, readFileSync, statSync, existsSync } from 'node:fs';
import { join, relative, sep } from 'node:path';

const root = process.argv[2];
if (!root || !existsSync(root)) {
	console.error('usage: docs-links-check.mjs <dist dir>');
	process.exit(2);
}

function walk(dir, out = []) {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) walk(p, out);
		else if (name.endsWith('.html')) out.push(p);
	}
	return out;
}

const pages = walk(root);
const ids = new Map(); // url path -> Set of ids
const links = []; // {from, href}

const hrefRe = /<a\s[^>]*?href="([^"#]*)(#[^"]*)?"/g;
const idRe = /\sid="([^"]+)"/g;
const nameRe = /<a\s[^>]*?name="([^"]+)"/g;

for (const file of pages) {
	const rel = '/' + relative(root, file).split(sep).join('/');
	const url = rel.endsWith('/index.html') ? rel.slice(0, -'index.html'.length) : rel;
	const html = readFileSync(file, 'utf8');
	const set = new Set();
	for (const m of html.matchAll(idRe)) set.add(m[1]);
	for (const m of html.matchAll(nameRe)) set.add(m[1]);
	ids.set(url, set);
	// Only links inside the article body. The sidebar and header are
	// generated from config, and a bad sidebar slug already fails the build.
	const body = html.match(/<main[\s\S]*?<\/main>/)?.[0] ?? html;
	for (const m of body.matchAll(hrefRe)) {
		const href = m[1];
		const hash = m[2] ?? '';
		if (/^(https?:|mailto:|tel:|javascript:)/.test(href)) continue;
		if (href === '' && hash === '') continue;
		links.push({ from: url, href, hash });
	}
}

function resolve(from, href) {
	if (href === '') return from;
	let p = href.split('?')[0];
	if (!p.startsWith('/')) {
		// relative link: resolve against the page directory
		const base = from.endsWith('/') ? from : from.slice(0, from.lastIndexOf('/') + 1);
		p = new URL(p, 'http://x' + base).pathname;
	}
	if (!p.endsWith('/') && !p.endsWith('.html') && !/\.[a-z0-9]+$/i.test(p)) p += '/';
	return p;
}

const failures = [];
for (const { from, href, hash } of links) {
	const target = resolve(from, href);
	// Non-HTML assets (images, files under /img/) are checked for existence on disk.
	if (/\.[a-z0-9]+$/i.test(target) && !target.endsWith('.html')) {
		if (!existsSync(join(root, target))) failures.push(`${from}: asset not found ${href}`);
		continue;
	}
	const set = ids.get(target);
	if (!set) {
		failures.push(`${from}: no page at ${href}`);
		continue;
	}
	if (hash && hash !== '#' && hash !== '#_top') {
		const id = decodeURIComponent(hash.slice(1));
		if (!set.has(id)) failures.push(`${from}: no anchor ${hash} on ${target}`);
	}
}

if (failures.length) {
	console.error(`docs-links-check: ${failures.length} broken link(s)`);
	for (const f of failures) console.error('  ' + f);
	process.exit(1);
}
console.log(`docs-links-check: ${links.length} links across ${pages.length} pages, all resolve.`);
