/**
 * The other Waired surfaces this documentation links out to, in one place so
 * a domain change is a one-line edit rather than a grep.
 *
 * SITE_URL is the marketing apex. The private repo's admin console still
 * defaults its own SITE_URL to the development apex while the production
 * domain is finalised (web/admin/src/lib/links.ts), but `docs.waired.ai` and
 * `app.waired.ai` are both live under the same apex, so this points at
 * `waired.ai`. If that apex is not the final one, change it here.
 */
export const SITE_URL = 'https://waired.ai';

/** The web console — sign-in, device list, account. Documented at /guides/web-console/. */
export const APP_URL = 'https://app.waired.ai';

/**
 * Header link labels, per locale. Starlight has no translation mechanism for
 * component-authored strings, so the two locales are spelled out here and
 * picked by `Astro.currentLocale`.
 */
export const HEADER_STRINGS = {
	root: { home: 'waired.ai', signIn: 'Sign in' },
	ja: { home: 'waired.ai', signIn: 'ログイン' },
} as const;

export function headerStrings(locale: string | undefined) {
	return locale === 'ja' ? HEADER_STRINGS.ja : HEADER_STRINGS.root;
}
