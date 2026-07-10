# Source-only helper: retry_with_backoff.
#
# Runs a command and retries it on non-zero exit, sleeping between
# attempts per a fixed backoff schedule. Built to absorb the transient
# `iamcredentials.googleapis.com` 503 ("upstream connect error /
# connection timeout") that the sv-mag self-hosted runner's
# Workload-Identity-Federation egress hits when terraform mints a
# federated access token during `init` / `apply` (issue #433). A single
# 503 there used to abort testnet teardown and leave the slot up.
#
# Backoff schedule: RETRY_BACKOFFS (default "0 15 30 60"), i.e. 4
# attempts, ~105s total — same budget as mint_id_token_with_retry in
# scripts/dev/testnet-fallback-runner.sh. The first entry is 0 (no sleep
# before the first attempt). On exhaustion the helper `exit`s 2.
#
# Usage (call as a BARE STATEMENT — see the set -e caveat below):
#
#   source "$ROOT/scripts/dev/lib/retry-backoff.sh"
#   retry_with_backoff "terraform init" -- \
#     terraform init -reconfigure -backend-config=... >/dev/null
#
# set -e caveat: under `set -e` this helper must be invoked as a bare
# statement, never as the condition of an `if`, nor on either side of
# `&&` / `||`, nor inside a pipeline. In those contexts bash suppresses
# `set -e` for the whole construct, so the internal `exit 2` on
# exhaustion degrades to a function return and the caller would sail past
# a real, unrecoverable failure. (mint_id_token_with_retry lives under
# the same constraint.)
#
# The command runs in the current shell, so redirections (`>/dev/null`)
# and shell builtins work as expected. Output streams from the command
# pass through untouched; only the retry/backoff notices go to stderr.

retry_with_backoff() {
  local label="${1:?retry_with_backoff: label required}"; shift
  if [[ "${1:-}" == "--" ]]; then
    shift
  fi
  if (( $# == 0 )); then
    echo "ERROR: retry_with_backoff: no command given" >&2
    exit 2
  fi

  local backoffs
  read -ra backoffs <<< "${RETRY_BACKOFFS:-0 15 30 60}"

  local attempt=0 rc=0 sleep_s
  for sleep_s in "${backoffs[@]}"; do
    if (( sleep_s > 0 )); then
      printf '   %s: retry in %ss\n' "${label}" "${sleep_s}" >&2
      sleep "${sleep_s}"
    fi
    attempt=$((attempt + 1))
    rc=0
    # The `|| rc=$?` is the set -e neutralizer: a command on the left of
    # `||` does not trip `set -e`, and $? captures the real exit code.
    # Keep this on one line — a bare `"$@"` would abort under set -e.
    "$@" || rc=$?
    if (( rc == 0 )); then
      (( attempt > 1 )) && printf '   %s: succeeded on attempt %d\n' "${label}" "${attempt}" >&2
      return 0
    fi
    printf '   %s: attempt %d/%d failed (rc=%d)\n' \
      "${label}" "${attempt}" "${#backoffs[@]}" "${rc}" >&2
  done

  echo "ERROR: ${label} failed after ${#backoffs[@]} attempts (last rc=${rc})" >&2
  exit 2
}
