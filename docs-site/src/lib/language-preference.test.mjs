// Runs src/lib/language-preference.js — the exact text Head.astro inlines —
// against fake browser globals, and checks where each reader ends up.
//
//   node --test src/lib/language-preference.test.mjs   (npm test)

import assert from 'node:assert/strict';
import fs from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const SOURCE = fs.readFileSync(new URL('./language-preference.js', import.meta.url), 'utf8');

// load runs the script once, as a page load at `url` would, and returns
// where it redirected (null when it stayed), the cookie it wrote, and the
// change listener it registered.
function load({ url, cookie = '', languages = ['en-US'], language = languages?.[0], cookieEnabled = true }) {
	const u = new URL(url, 'https://docs.waired.ai');
	const result = { redirect: null, written: null, listener: null, capture: null };
	const document = {
		addEventListener(type, fn, capture) {
			assert.equal(type, 'change');
			result.listener = fn;
			result.capture = capture;
		},
		get cookie() {
			return cookie;
		},
		set cookie(v) {
			result.written = v;
		},
	};
	const location = {
		pathname: u.pathname,
		search: u.search,
		hash: u.hash,
		protocol: u.protocol,
		replace(to) {
			assert.equal(result.redirect, null, 'redirected twice in one load');
			result.redirect = to;
		},
	};
	const navigator = { languages, language, cookieEnabled };
	vm.runInNewContext(SOURCE, { document, location, navigator });
	return result;
}

// pick fires the registered listener the way the language picker does.
function pick(listener, value, { inPicker = true } = {}) {
	listener({
		target: {
			tagName: 'SELECT',
			value,
			closest: (sel) => (inPicker && sel === 'starlight-lang-select' ? {} : null),
		},
	});
}

const JA_BROWSER = ['ja-JP', 'ja', 'en-US'];
const EN_BROWSER = ['en-US', 'en'];

test('a Japanese browser that has not picked is sent from an English URL to /ja/', () => {
	assert.equal(load({ url: '/', languages: JA_BROWSER }).redirect, '/ja/');
	assert.equal(load({ url: '/quickstart/', languages: JA_BROWSER }).redirect, '/ja/quickstart/');
	assert.equal(
		load({ url: '/reference/cli/?q=1#waired-init', languages: JA_BROWSER }).redirect,
		'/ja/reference/cli/?q=1#waired-init',
	);
});

test('the first of English or Japanese in the browser list decides', () => {
	assert.equal(load({ url: '/quickstart/', languages: ['en-US', 'ja'] }).redirect, null);
	assert.equal(load({ url: '/quickstart/', languages: ['zh-CN', 'ja-JP', 'en'] }).redirect, '/ja/quickstart/');
	assert.equal(load({ url: '/quickstart/', languages: ['fr-FR', 'de'] }).redirect, null);
	assert.equal(load({ url: '/quickstart/', languages: ['JA'] }).redirect, '/ja/quickstart/');
	// navigator.languages missing or empty: navigator.language.
	assert.equal(load({ url: '/quickstart/', languages: null, language: 'ja-JP' }).redirect, '/ja/quickstart/');
	assert.equal(load({ url: '/quickstart/', languages: [], language: 'ja' }).redirect, '/ja/quickstart/');
	assert.equal(load({ url: '/quickstart/', languages: [], language: 'en-GB' }).redirect, null);
});

test('an English browser that has not picked stays where the link points', () => {
	assert.equal(load({ url: '/quickstart/', languages: EN_BROWSER }).redirect, null);
	assert.equal(load({ url: '/ja/quickstart/', languages: EN_BROWSER }).redirect, null);
});

test('a /ja/ URL is never sent to English by the browser language alone', () => {
	assert.equal(load({ url: '/ja/quickstart/', languages: JA_BROWSER }).redirect, null);
	assert.equal(load({ url: '/ja/', languages: EN_BROWSER }).redirect, null);
});

test('a reader who picked English reads English, even with a Japanese browser', () => {
	const cookie = 'theme=dark; waired-docs-lang=en';
	assert.equal(load({ url: '/quickstart/', cookie, languages: JA_BROWSER }).redirect, null);
	assert.equal(load({ url: '/ja/quickstart/', cookie, languages: JA_BROWSER }).redirect, '/quickstart/');
	assert.equal(load({ url: '/ja/', cookie, languages: JA_BROWSER }).redirect, '/');
	assert.equal(load({ url: '/ja', cookie, languages: JA_BROWSER }).redirect, '/');
});

test('a reader who picked Japanese reads Japanese, even with an English browser', () => {
	const cookie = 'waired-docs-lang=ja';
	assert.equal(load({ url: '/quickstart/', cookie, languages: EN_BROWSER }).redirect, '/ja/quickstart/');
	assert.equal(load({ url: '/ja/quickstart/', cookie, languages: EN_BROWSER }).redirect, null);
});

test('an English path that only starts with "ja" is English', () => {
	assert.equal(load({ url: '/japan/', cookie: 'waired-docs-lang=en' }).redirect, null);
	assert.equal(load({ url: '/japan/', cookie: 'waired-docs-lang=ja' }).redirect, '/ja/japan/');
});

test('with cookies off, the browser language does not redirect', () => {
	// The pick could not be remembered, so the redirect would undo every
	// choice of English.
	assert.equal(load({ url: '/quickstart/', languages: JA_BROWSER, cookieEnabled: false }).redirect, null);
});

test('an unknown cookie value counts as no pick', () => {
	assert.equal(load({ url: '/quickstart/', cookie: 'waired-docs-lang=fr', languages: EN_BROWSER }).redirect, null);
	assert.equal(load({ url: '/quickstart/', cookie: 'xwaired-docs-lang=ja', languages: EN_BROWSER }).redirect, null);
});

test('picking in the language picker writes the cookie, before the picker navigates', () => {
	const page = load({ url: '/quickstart/', languages: JA_BROWSER, cookie: 'waired-docs-lang=en' });
	assert.equal(page.capture, true, 'must run in the capture phase, before the picker navigates');

	pick(page.listener, '/ja/quickstart/');
	assert.equal(page.written, 'waired-docs-lang=ja; Max-Age=31536000; Path=/; SameSite=Lax; Secure');

	pick(page.listener, '/quickstart/');
	assert.equal(page.written, 'waired-docs-lang=en; Max-Age=31536000; Path=/; SameSite=Lax; Secure');

	pick(page.listener, '/');
	assert.equal(page.written, 'waired-docs-lang=en; Max-Age=31536000; Path=/; SameSite=Lax; Secure');
});

test('other selects on the page do not write the cookie', () => {
	const page = load({ url: '/quickstart/' });
	pick(page.listener, '/ja/quickstart/', { inPicker: false });
	assert.equal(page.written, null);
	page.listener({ target: { tagName: 'INPUT', value: 'ja', closest: () => ({}) } });
	assert.equal(page.written, null);
});

test('the cookie is not marked Secure on plain http (the local preview)', () => {
	const page = load({ url: 'http://localhost:4321/quickstart/' });
	pick(page.listener, '/ja/quickstart/');
	assert.equal(page.written, 'waired-docs-lang=ja; Max-Age=31536000; Path=/; SameSite=Lax');
});

test('loading the page redirected to never redirects again', () => {
	// Every reader state, every kind of URL: the redirect target is a page
	// that stays put, so no combination can loop.
	for (const cookie of ['', 'waired-docs-lang=en', 'waired-docs-lang=ja']) {
		for (const languages of [JA_BROWSER, EN_BROWSER]) {
			for (const url of ['/', '/quickstart/', '/ja', '/ja/', '/ja/quickstart/', '/japan/']) {
				const first = load({ url, cookie, languages });
				if (first.redirect === null) continue;
				const second = load({ url: first.redirect, cookie, languages });
				assert.equal(second.redirect, null, `${url} (${cookie || 'no pick'}, ${languages[0]}) → ${first.redirect} → ${second.redirect}`);
			}
		}
	}
});
