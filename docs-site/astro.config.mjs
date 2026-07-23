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
			// The header has to say what this site IS. A reader arriving on a
			// deep page from a search engine sees only the header, and "Waired"
			// alone does not distinguish the documentation from the product
			// site or the console.
			//
			// Not localised, on purpose: "Docs" is how the product names
			// itself, and ドキュメント in the chrome reads worse than leaving
			// it. Starlight's own header UI keeps its built-in translations,
			// so the Japanese tree still says 検索.
			title: 'Waired Docs',
			// The GATE mark, identical to the marketing site's and the admin
			// console's favicons. It carries its own dark chip, so one asset
			// serves both site themes — see src/assets/waired-mark.svg.
			logo: {
				src: './src/assets/waired-mark.svg',
				alt: 'Waired',
				replacesTitle: false,
			},
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
			components: {
				// Two site-wide conventions the stock component has no slot
				// for: the per-page header block (`meta` frontmatter) and the
				// Japanese translation-freshness notice.
				PageTitle: './src/components/PageTitle.astro',
				// Starlight's header has no slot for product navigation, and
				// SocialIcons is the only component rendered there — so the
				// links out to waired.ai and the console live in it. The
				// header's right group is desktop-only, hence the matching
				// MobileMenuFooter override; keep the two in step.
				SocialIcons: './src/components/SocialIcons.astro',
				MobileMenuFooter: './src/components/MobileMenuFooter.astro',
			},
			// Explicit `slug` entries (not autogenerate) so order and labels
			// are intentional and a typo'd slug fails the build. Slugs are
			// locale-agnostic — Starlight prepends the active locale.
			// PROTOTYPE sidebar (docs IA revision). Reordered around the
			// journey a user actually walks — start → set up → use → fix →
			// understand → look up — with labels written as the thing the
			// reader wants, not the feature name.
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'What is Waired?', slug: 'what-is-waired' },
						{ label: 'Quickstart — your first answer', slug: 'quickstart' },
						// Third, not last: on a desktop the app IS Waired, so a
						// reader who finishes the Quickstart needs this before
						// any of the task guides make sense.
						{ label: 'The Waired app', slug: 'guides/waired-app' },
					],
				},
				{
					label: 'Set it up',
					items: [
						{ label: 'Install', slug: 'getting-started/install' },
						{ label: 'Install on Windows', slug: 'getting-started/install/windows' },
						{ label: 'Install on macOS', slug: 'getting-started/install/macos' },
						{ label: 'Install on Linux', slug: 'getting-started/install/linux' },
						{ label: 'Sign in and set up', slug: 'getting-started/first-run' },
						{ label: 'Check it works', slug: 'getting-started/verify' },
						{ label: 'Add another device', slug: 'getting-started/add-a-device' },
						{ label: 'Update Waired', slug: 'getting-started/update' },
						{ label: 'Uninstall', slug: 'getting-started/uninstall' },
					],
				},
				{
					label: 'Use your AI',
					items: [
						{ label: 'Use it from Claude Code', slug: 'guides/claude-code' },
						{ label: 'Use it from OpenCode', slug: 'guides/opencode' },
						{ label: 'Use it from OpenClaw', slug: 'guides/openclaw' },
						{ label: 'Use it from a chat app', slug: 'guides/chat-clients' },
						{ label: 'Choose which AI model runs', slug: 'guides/choose-a-model' },
						{ label: 'Stop using your AI for a while', slug: 'guides/pause' },
						{ label: 'The web console', slug: 'guides/web-console' },
						{ label: 'Share it with other people', slug: 'public-share' },
					],
				},
				{
					label: 'When something looks wrong',
					items: [
						// First: it is the one command that resolves most
						// first-run problems, and every other page points here.
						{ label: 'Run a health check', slug: 'getting-started/doctor' },
						{ label: 'Troubleshooting', slug: 'troubleshooting' },
						{ label: 'FAQ', slug: 'faq' },
					],
				},
				{
					label: 'How it works',
					items: [
						{ label: 'Privacy — what leaves your computer', slug: 'concepts/privacy' },
						{ label: 'Architecture', slug: 'concepts/architecture' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Words used in this documentation', slug: 'reference/glossary' },
						{ label: 'CLI commands', slug: 'reference/cli' },
						{ label: 'Model catalog & specs', slug: 'reference/model-catalog' },
						{ label: 'Advanced install options', slug: 'reference/install-options' },
						{ label: "What's new", slug: 'reference/release-notes' },
					],
				},
			],
		}),
	],
});
