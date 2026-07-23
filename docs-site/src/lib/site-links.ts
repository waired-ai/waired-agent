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
 * The header chrome is deliberately NOT localised: "Docs" and "Sign in" are
 * how the product names itself, and transliterating them into katakana reads
 * worse than leaving them in English. This applies only to strings this repo
 * authors — Starlight's own header UI (search, theme, language) keeps its
 * built-in translations, so the Japanese tree still says 検索.
 */
export const HEADER_LABELS = {
	home: 'waired.ai',
	signIn: 'Sign in',
} as const;
