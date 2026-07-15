// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Published to Firebase Hosting at the apex (https://docs.waired.ai/).
// `main` deploys to the live channel; each PR gets an auto-expiring
// preview channel (*.web.app). Hosting is at the domain root, so there
// is no `base` subpath — internal links are root-relative (`/...`,
// `/ja/...`). See .github/workflows/deploy-docs.yml + docs-site/firebase.json.
// https://astro.build/config
export default defineConfig({
	site: 'https://docs.waired.ai',
	base: '/',
	integrations: [
		starlight({
			title: 'Waired',
			// English is the canonical/base language and sits at the site
			// root (`/...`); Japanese is a mirror under `/ja/...`.
			// Untranslated `ja` pages fall back to English automatically, so
			// the Japanese tree can fill in page by page.
			defaultLocale: 'root',
			locales: {
				root: { label: 'English', lang: 'en' },
				ja: { label: '日本語', lang: 'ja' },
			},
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/waired-ai/waired' },
			],
			// Explicit `slug` entries (not autogenerate) so order and labels
			// are intentional and a typo'd slug fails the build. Slugs are
			// locale-agnostic — Starlight prepends the active locale.
			sidebar: [
				{
					label: 'Getting started',
					items: [
						{ label: 'Install', slug: 'getting-started/install' },
						{ label: 'First run', slug: 'getting-started/first-run' },
						{ label: 'Verify it works', slug: 'getting-started/verify' },
					],
				},
				{
					label: 'Guides',
					items: [
						{ label: 'Coding agents (Claude Code / OpenCode)', slug: 'guides/coding-agents' },
						{ label: 'Chat & OpenAI-compatible clients', slug: 'guides/chat-clients' },
						{ label: 'Switch the bundled model', slug: 'guides/switch-model' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'CLI commands', slug: 'reference/cli' },
						{ label: 'Model catalog & specs', slug: 'reference/model-catalog' },
						{ label: 'Advanced install options', slug: 'reference/install-options' },
					],
				},
				{
					label: 'How it works',
					items: [
						{ label: 'Architecture', slug: 'concepts/architecture' },
						{ label: 'Privacy', slug: 'concepts/privacy' },
					],
				},
				{ label: 'Troubleshooting', slug: 'troubleshooting' },
				{ label: 'FAQ', slug: 'faq' },
			],
		}),
	],
});
