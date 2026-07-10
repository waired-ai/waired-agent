# Security Policy

Waired runs a WireGuard data plane, intercepts coding-agent traffic through a
local gateway, and operates a hosted control plane — we take vulnerability
reports seriously and appreciate responsible disclosure.

## Reporting a vulnerability

Email **security@waired.ai** with:

- A description of the issue and its impact.
- Steps to reproduce (proof-of-concept if available).
- Affected component and version (`waired --version`), if known.

Please do **not** open a public GitHub issue for security problems.

You will receive an acknowledgement within 7 days. Please allow up to 90 days
for a fix before public disclosure; we will coordinate the timeline with you
if a fix needs longer.

## Scope

- The `waired` CLI, agent, tray, and installers (Linux / macOS / Windows).
- The local gateway and Claude Code / OpenCode integration (loopback proxy).
- The hosted control plane and relay operated by the waired project.
- The distribution pipeline (install script, APT repository, release
  artifacts).

Out of scope: vulnerabilities in third-party software we integrate with
(Ollama, vLLM, Claude Code, OpenCode) — report those upstream — and issues
requiring physical access to an already-compromised device.

## Rewards

This is an individually operated project; there is currently no bug bounty
program. We will credit reporters in release notes on request.
