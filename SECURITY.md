# Security Policy

Waired runs a WireGuard data plane, intercepts coding-agent traffic through a
local gateway, and operates a hosted control plane. Vulnerability reports are
welcome, and responsible disclosure is appreciated.

## Reporting a vulnerability

Email **security@waired.ai** with:

- A description of the issue and its impact.
- Steps to reproduce (proof-of-concept if available).
- Affected component and version (`waired version`), if known.

Do **not** open a public GitHub issue for security problems.

You will receive an acknowledgement within 7 days. Allow up to 90 days for a
fix before public disclosure; if a fix needs longer, the timeline is
coordinated with you.

## Scope

- The `waired` CLI, the agent, the desktop app, and the installers (Linux / macOS / Windows).
- The local gateway and Claude Code / OpenCode / OpenClaw integration (loopback proxy).
- The hosted control plane and relay operated by the waired project.
- The distribution pipeline (install script, APT repository, release
  artifacts).

Out of scope: vulnerabilities in third-party software Waired integrates with
(Ollama, vLLM, Claude Code, OpenCode, OpenClaw) — report those upstream — and
issues that require physical access to an already-compromised computer.

## Rewards

This project is run by a single maintainer; there is no bug bounty program
at present. Reporters are credited in the release notes on request.
