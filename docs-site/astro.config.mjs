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
	// Old URLs that the docs themselves, search engines, or a reader's
	// bookmarks may still carry. Astro emits a meta-refresh page for each;
	// firebase.json carries the same list as real 301s. Product-printed URLs
	// (/quickstart/, /public-share/, /reference/cli/, /reference/model-catalog/,
	// /reference/install-options/) never move, so they are not listed here.
	redirects: {
		'/getting-started/first-run/': '/getting-started/sign-in/',
		'/ja/getting-started/first-run/': '/ja/getting-started/sign-in/',
		'/guides/models/': '/guides/choose-a-model/',
		'/ja/guides/models/': '/ja/guides/choose-a-model/',
		'/guides/public-share/': '/public-share/',
		'/ja/guides/public-share/': '/ja/public-share/',
	},
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
			// locale-agnostic — Starlight prepends the active locale, and
			// every entry carries its Japanese label in `translations`.
			//
			// The order is the journey a reader walks: understand → install
			// and set up → use it from a tool → models and routing → the app
			// and console → fix a problem → how it works → look something up.
			// Inside a group the order is concept → task → reference, and
			// within tasks, first-time setup before day-to-day use.
			sidebar: [
				{
					label: 'Get started',
					translations: { ja: 'はじめる' },
					items: [
						{ label: 'What is Waired?', translations: { ja: 'Wairedとは' }, slug: 'what-is-waired' },
						{ label: 'Quickstart', translations: { ja: 'クイックスタート' }, slug: 'quickstart' },
						{ label: 'Meet the Waired app', translations: { ja: 'Wairedアプリの基本' }, slug: 'getting-started/meet-the-app' },
						{ label: 'Check that it works', translations: { ja: '動作を確認する' }, slug: 'getting-started/verify' },
						{ label: 'FAQ', translations: { ja: 'よくある質問' }, slug: 'faq' },
					],
				},
				{
					label: 'Install and set up',
					translations: { ja: 'インストールとセットアップ' },
					items: [
						{ label: 'Install Waired', translations: { ja: 'Wairedをインストールする' }, slug: 'getting-started/install' },
						{ label: 'Install on Windows', translations: { ja: 'Windowsにインストールする' }, slug: 'getting-started/install/windows' },
						{ label: 'Install on macOS', translations: { ja: 'macOSにインストールする' }, slug: 'getting-started/install/macos' },
						{ label: 'Install on Linux', translations: { ja: 'Linuxにインストールする' }, slug: 'getting-started/install/linux' },
						{ label: 'Sign in', translations: { ja: 'サインインする' }, slug: 'getting-started/sign-in' },
						{ label: 'Set up in the browser', translations: { ja: 'ブラウザでセットアップする' }, slug: 'getting-started/set-up-in-the-browser' },
						{ label: 'Set up in the terminal', translations: { ja: 'ターミナルでセットアップする' }, slug: 'getting-started/set-up-in-the-terminal' },
						{ label: 'Set up a server with an auth key', translations: { ja: '認証キーでサーバーをセットアップする' }, slug: 'getting-started/servers-and-auth-keys' },
						{ label: 'Run setup again', translations: { ja: 'セットアップをやり直す' }, slug: 'getting-started/set-up-again' },
						{ label: 'Add another computer', translations: { ja: '別のパソコンを追加する' }, slug: 'getting-started/add-a-device' },
						{ label: 'Update Waired', translations: { ja: 'Wairedをアップデートする' }, slug: 'getting-started/update' },
						{ label: 'Uninstall Waired', translations: { ja: 'Wairedをアンインストールする' }, slug: 'getting-started/uninstall' },
					],
				},
				{
					label: 'Use it from your tools',
					translations: { ja: 'ツールから使う' },
					items: [
						{ label: 'Use Waired from Claude Code', translations: { ja: 'Claude Codeから使う' }, slug: 'guides/claude-code' },
						{ label: 'Use Waired from OpenCode', translations: { ja: 'OpenCodeから使う' }, slug: 'guides/opencode' },
						{ label: 'Use Waired from OpenClaw', translations: { ja: 'OpenClawから使う' }, slug: 'guides/openclaw' },
						{ label: 'Use Waired from a chat app', translations: { ja: 'チャットアプリから使う' }, slug: 'guides/chat-clients' },
					],
				},
				{
					label: 'Models and routing',
					translations: { ja: 'モデルとルーティング' },
					items: [
						{ label: 'Change the model', translations: { ja: 'モデルを変更する' }, slug: 'guides/choose-a-model' },
						{ label: 'Pause or stop Waired', translations: { ja: 'Wairedを一時停止する' }, slug: 'guides/pause' },
						{ label: 'Public Share', translations: { ja: 'パブリック共有' }, slug: 'public-share' },
					],
				},
				{
					label: 'The Waired app and console',
					translations: { ja: 'Wairedアプリとコンソール' },
					items: [
						{ label: 'The Waired app menu', translations: { ja: 'Wairedアプリのメニュー' }, slug: 'guides/waired-app' },
						{ label: 'The web console', translations: { ja: 'Webコンソール' }, slug: 'guides/web-console' },
					],
				},
				{
					label: 'Fix a problem',
					translations: { ja: '問題を解決する' },
					items: [
						{ label: 'Run a health check', translations: { ja: '診断を実行する' }, slug: 'getting-started/doctor' },
						{ label: 'Troubleshooting', translations: { ja: 'トラブルシューティング' }, slug: 'troubleshooting' },
						{ label: 'Report a problem', translations: { ja: '不具合を報告する' }, slug: 'getting-started/report-a-problem' },
					],
				},
				{
					label: 'How it works',
					translations: { ja: '仕組み' },
					items: [
						{ label: 'Privacy: what leaves your computer', translations: { ja: 'プライバシー：パソコンの外に出るもの' }, slug: 'concepts/privacy' },
						{ label: 'Architecture', translations: { ja: 'アーキテクチャ' }, slug: 'concepts/architecture' },
					],
				},
				{
					label: 'Reference',
					translations: { ja: 'リファレンス' },
					items: [
						{ label: 'CLI commands', translations: { ja: 'CLIコマンド' }, slug: 'reference/cli' },
						{ label: 'Model catalog', translations: { ja: 'モデルカタログ' }, slug: 'reference/model-catalog' },
						{ label: 'Advanced install options', translations: { ja: '高度なインストールオプション' }, slug: 'reference/install-options' },
						{ label: 'Glossary', translations: { ja: '用語集' }, slug: 'reference/glossary' },
						{ label: "What's new", translations: { ja: '更新情報' }, slug: 'reference/release-notes' },
					],
				},
			],
		}),
	],
});
