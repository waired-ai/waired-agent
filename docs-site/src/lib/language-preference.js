// Which language a docs page opens in. Inlined into every page's <head> by
// src/components/Head.astro, so it runs before anything is drawn.
//
// English sits at the site root and Japanese under /ja/ (astro.config.mjs).
//
//   * A reader who picks a language in the header's language picker has it
//     remembered in a cookie, and that choice wins on every URL: an English
//     URL opens in Japanese for a reader who picked 日本語, and a /ja/ URL
//     opens in English for one who picked English.
//   * Until the reader picks, a browser that lists Japanese before English
//     is sent from an English URL to the same page under /ja/. A /ja/ URL is
//     left alone: the link already asked for Japanese.
//
// Only the picker writes the cookie. Arriving through the redirect does not,
// so a reader who changes their browser's language is followed until they
// pick one here.
//
// Plain script, not a module: Head.astro inlines it as text. Tested by
// language-preference.test.mjs, which runs this file against fake globals.
(function () {
	var COOKIE = 'waired-docs-lang';
	var JA = '/ja';

	function isJapanesePath(path) {
		return path === JA || path.indexOf(JA + '/') === 0;
	}

	// The first of English or Japanese in the browser's language list. The
	// order is the reader's; a language the docs are not written in is
	// skipped rather than read as a vote for English.
	function browserPrefersJapanese() {
		var langs = navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language];
		for (var i = 0; i < langs.length; i++) {
			var primary = String(langs[i] || '').toLowerCase().split('-')[0];
			if (primary === 'ja') return true;
			if (primary === 'en') return false;
		}
		return false;
	}

	// Starlight's picker (<starlight-lang-select>, in the header and in the
	// mobile menu) navigates from its own change listener. Capture runs first,
	// so the cookie is written before the next page loads and reads it.
	document.addEventListener(
		'change',
		function (event) {
			var select = event.target;
			if (!select || select.tagName !== 'SELECT' || !select.closest('starlight-lang-select')) return;
			var lang = isJapanesePath(select.value) ? 'ja' : 'en';
			document.cookie =
				COOKIE + '=' + lang + '; Max-Age=31536000; Path=/; SameSite=Lax' + (location.protocol === 'https:' ? '; Secure' : '');
		},
		true,
	);

	var match = /(?:^|;\s*)waired-docs-lang=(en|ja)(?:;|$)/.exec(document.cookie);
	var chosen = match ? match[1] : null;
	var path = location.pathname;
	var target = null;
	if (isJapanesePath(path)) {
		if (chosen === 'en') target = path.slice(JA.length) || '/';
	} else if (chosen === 'ja') {
		target = JA + path;
	} else if (!chosen && navigator.cookieEnabled && browserPrefersJapanese()) {
		// With cookies off the picker's choice cannot be remembered, and this
		// redirect would undo every pick of English. Stay on the URL instead.
		target = JA + path;
	}
	if (target !== null) location.replace(target + location.search + location.hash);
})();
