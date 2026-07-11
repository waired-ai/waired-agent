#!/usr/bin/env bash
# Shared helpers for the catalog-radar pipeline (issue #413).
# Sourced by run.sh and the M5 gh wrappers; no side effects on source.

# log writes a timestamped progress line to stderr (stdout stays for data).
log() { printf '[catalog-radar] %s\n' "$*" >&2; }

# die logs and exits non-zero.
die() {
  printf '[catalog-radar] ERROR: %s\n' "$*" >&2
  exit 1
}

# require_env fails fast if a named environment variable is empty.
require_env() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    die "required environment variable ${name} is not set"
  fi
}

# have_cmd reports whether a command is on PATH.
have_cmd() { command -v "$1" >/dev/null 2>&1; }
