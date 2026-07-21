# Waired documentation site

Public help/documentation site for Waired, built with
[Astro](https://astro.build/) + [Starlight](https://starlight.astro.build/)
and published to Firebase Hosting at **https://docs.waired.ai/**.

English is the canonical language (site root); Japanese is a mirror under
`ja/` that falls back to English page-by-page.

## Develop

```sh
npm install
npm run dev       # local dev server
npm run build     # production build into dist/
npm run preview   # preview the built site (served at /)
```

## Deploy

`.github/workflows/deploy-docs.yml` builds this directory and deploys to
Firebase Hosting:

- **push to `main`** → live channel (`docs.waired.ai`), served from the prod
  GCP project **`prod-waired`** (Firebase site `waired-docs-prod`).
- **pull request** (same-repo) → an auto-expiring preview channel in the
  **`dev-waired`** sandbox (Firebase site `waired-docs`), linked back as a PR
  comment.

PRs are build-checked by the build step in `.github/workflows/deploy-docs.yml`
(the build runs on every docs PR even when the deploy steps are skipped).

Auth is Workload Identity Federation (OIDC) + `firebase-tools` via ADC — no
long-lived service-account key secret. The deploy steps are gated on the
`FIREBASE_PROJECT_ID` variable, so the workflow is merge-safe before the
operator provisions Firebase (until then it only builds). Live vs preview
project is selected by a conditional `environment:` on the deploy job: `push`
runs in the GitHub `production` Environment (prod vars), `pull_request` uses the
repo-level dev vars (#453). `firebase.json` uses `target: docs`, resolved per
project by `.firebaserc`. See `docs/decisions.md` for the hosting decision.

## Layout

- `src/content/docs/` — English pages (Markdown / MDX); `src/content/docs/ja/`
  — Japanese mirror.
- `firebase.json` / `.firebaserc` — Firebase Hosting config (`target: docs` →
  `waired-docs-prod` in prod-waired / `waired-docs` in dev-waired, publishes
  `dist/`).
- `astro.config.mjs` — site config; hosted at the domain apex (`base: '/'`).
