#!/usr/bin/env node
// docs-preview-channels.mjs — the decision half of docs-preview-sweep.sh (#429).
//
// Everything here is JSON in / lines out with no side effects on Firebase, so
// the rule that decides "which preview channels may be deleted" is testable
// without touching shared infrastructure (scripts/ci/docs-preview-select-test.sh).
// The sibling shell script owns the firebase CLI invocations.
//
// Subcommands
//   site       env FIREBASE_PROJECT_ID [, FIREBASERC] -> the Hosting site id
//              for that project, read from docs-site/.firebaserc's `docs`
//              target. Nothing hardcodes "waired-docs".
//   open-prs   env GH_TOKEN|GITHUB_TOKEN [, GITHUB_REPOSITORY, GITHUB_API_URL]
//              -> a JSON array of open PR numbers. Exits non-zero on ANY API
//              failure and prints nothing: a partial or empty-by-accident list
//              would mark every live preview as reapable.
//   select     stdin `firebase hosting:channel:list --json` output
//              env OPEN_PRS (JSON array) or PR_ONLY [, NOW]
//              -> the channel ids to delete, one per line.
//
// Safety rules `select` enforces (a preview channel lives in a shared project;
// `live` serves https://docs.waired.ai):
//   * only ids matching /^pr-\d+$/ are ever emitted — `live` and any
//     hand-made channel are invisible to this script by construction;
//   * sweep mode requires OPEN_PRS to PARSE as an array. Unset or malformed is
//     an error, never "no PRs are open, delete everything". `[]` is a valid
//     answer and does mean "no open PRs";
//   * an expired channel is emitted whatever its PR's state: its URL is
//     already dead, and the next push to an open PR recreates it.

import { readFileSync } from "node:fs";

const die = (msg) => {
  process.stderr.write(`docs-preview-channels: ${msg}\n`);
  process.exit(1);
};

function cmdSite() {
  const project = process.env.FIREBASE_PROJECT_ID || "";
  if (!project) die("site: FIREBASE_PROJECT_ID is required");
  const path = process.env.FIREBASERC || "docs-site/.firebaserc";
  let rc;
  try {
    rc = JSON.parse(readFileSync(path, "utf8"));
  } catch (err) {
    die(`site: cannot read ${path}: ${err.message}`);
  }
  const sites = rc?.targets?.[project]?.hosting?.docs;
  if (!Array.isArray(sites) || typeof sites[0] !== "string" || !sites[0]) {
    die(`site: ${path} has no targets.${project}.hosting.docs[0]`);
  }
  process.stdout.write(`${sites[0]}\n`);
}

async function cmdOpenPrs() {
  const token = process.env.GH_TOKEN || process.env.GITHUB_TOKEN || "";
  if (!token) die("open-prs: GH_TOKEN or GITHUB_TOKEN is required");
  const repo = process.env.GITHUB_REPOSITORY || "waired-ai/waired-agent";
  const api = process.env.GITHUB_API_URL || "https://api.github.com";

  const numbers = [];
  // Page rather than trusting one request to cover every open PR: this repo
  // routinely carries more than a page's worth.
  for (let page = 1; page <= 20; page++) {
    const url = `${api}/repos/${repo}/pulls?state=open&per_page=100&page=${page}`;
    let res;
    try {
      res = await fetch(url, {
        headers: {
          accept: "application/vnd.github+json",
          authorization: `Bearer ${token}`,
          "x-github-api-version": "2022-11-28",
          "user-agent": "waired-docs-preview-sweep",
        },
      });
    } catch (err) {
      die(`open-prs: ${url} failed: ${err.message}`);
    }
    if (!res.ok) die(`open-prs: ${url} -> HTTP ${res.status}`);
    const batch = await res.json();
    if (!Array.isArray(batch)) die("open-prs: unexpected response shape");
    for (const pr of batch) numbers.push(pr.number);
    if (batch.length < 100) {
      process.stdout.write(`${JSON.stringify(numbers)}\n`);
      return;
    }
  }
  die("open-prs: more pages than expected; refusing a truncated list");
}

function readStdin() {
  try {
    return readFileSync(0, "utf8");
  } catch (err) {
    die(`select: cannot read stdin: ${err.message}`);
  }
}

function cmdSelect() {
  const raw = readStdin();
  let doc;
  try {
    doc = JSON.parse(raw);
  } catch (err) {
    die(`select: stdin is not JSON: ${err.message}`);
  }
  // `firebase --json` wraps its payload in {status, result}; accept a bare
  // {channels} too so the selector can be driven straight from the REST API.
  const channels = doc?.result?.channels ?? doc?.channels;
  if (!Array.isArray(channels)) die("select: no channels array in input");

  const prOnly = process.env.PR_ONLY || "";
  let open = null;
  if (!prOnly) {
    const rawOpen = process.env.OPEN_PRS;
    if (rawOpen === undefined || rawOpen === "") {
      die("select: OPEN_PRS is required in sweep mode (use PR_ONLY for one PR)");
    }
    let parsed;
    try {
      parsed = JSON.parse(rawOpen);
    } catch (err) {
      die(`select: OPEN_PRS is not JSON: ${err.message}`);
    }
    if (!Array.isArray(parsed)) die("select: OPEN_PRS must be a JSON array");
    open = new Set(parsed.map(Number));
  }

  const nowRaw = process.env.NOW;
  const now = nowRaw ? new Date(nowRaw) : new Date();
  if (Number.isNaN(now.getTime())) die(`select: NOW is not a date: ${nowRaw}`);

  const out = [];
  for (const ch of channels) {
    const id = String(ch?.name || "").split("/").pop();
    const m = /^pr-(\d+)$/.exec(id);
    if (!m) continue; // `live` and anything hand-made: never touched.
    const number = Number(m[1]);

    if (prOnly) {
      if (String(number) === String(Number(prOnly))) out.push(id);
      continue;
    }

    const expiry = ch?.expireTime ? new Date(ch.expireTime) : null;
    const expired = expiry !== null && !Number.isNaN(expiry.getTime()) && expiry < now;
    if (expired || !open.has(number)) out.push(id);
  }
  if (out.length) process.stdout.write(`${out.join("\n")}\n`);
}

const sub = process.argv[2];
if (sub === "site") cmdSite();
else if (sub === "open-prs") await cmdOpenPrs();
else if (sub === "select") cmdSelect();
else die(`usage: docs-preview-channels.mjs <site|open-prs|select>`);
