#!/usr/bin/env bash
# testnet-cloudlogging-poll.sh — sourceable helper that abstracts the
# "gcloud logging read | python3 parse" pattern used by both
# testnet-punch-verify.sh (verify gate) and testnet-fallback-runner.sh
# (fallback gate).
#
# Exports two functions:
#
#   cloudlog_fetch FILTER FRESHNESS FIELDS
#       One-shot read of Cloud Logging. Returns the gcloud --format
#       JSON on stdout. FIELDS is a gcloud --format projection (e.g.
#       'json(labels.instance_name,jsonPayload.state,timestamp)').
#       Exit 0 on success regardless of result count; stderr is
#       suppressed.
#
#   cloudlog_poll_until FILTER FRESHNESS FIELDS TIMEOUT INTERVAL CHECK_PY
#       Polls FILTER every INTERVAL seconds up to TIMEOUT. CHECK_PY is
#       a Python source fragment that reads the gcloud JSON from stdin
#       (`raw = sys.stdin.read(); data = json.loads(raw)`), and
#       *prints* one line: "OK\n" when the predicate is satisfied,
#       "WAIT: <reason>\n" otherwise. cloudlog_poll_until returns 0
#       once "OK" is observed, 1 on timeout.
#
# IMPORTANT: the python invocation uses `python3 -c "$(cat <<'PY'..)"`
# NOT `python3 - <<'PY'`. The latter pattern silently discards the
# piped JSON because the heredoc wins the stdin redirect; -c keeps
# stdin available for the gcloud pipe. The same gotcha is documented
# in testnet-punch-verify.sh.

# shellcheck disable=SC2120
cloudlog_fetch() {
  local filter="$1" freshness="$2" fields="$3"
  : "${PROJECT_ID:?PROJECT_ID must be set}"
  gcloud logging read "$filter" \
    --project="$PROJECT_ID" \
    --freshness="$freshness" \
    --order=desc \
    --limit=200 \
    --format="$fields" \
    2>/dev/null
}

cloudlog_poll_until() {
  local filter="$1" freshness="$2" fields="$3" timeout_s="$4" interval_s="$5" check_py="$6"
  local start now elapsed json verdict
  start=$(date +%s)
  while :; do
    json="$(cloudlog_fetch "$filter" "$freshness" "$fields")"
    verdict="$(printf '%s' "$json" | python3 -c "$check_py")"
    case "$verdict" in
      OK*)
        return 0
        ;;
      WAIT*)
        ;;
      *)
        echo "  cloudlog_poll_until: unexpected verdict: ${verdict}" >&2
        ;;
    esac
    now=$(date +%s)
    elapsed=$(( now - start ))
    if (( elapsed >= timeout_s )); then
      echo "  cloudlog_poll_until: timeout after ${elapsed}s; last verdict: ${verdict}" >&2
      return 1
    fi
    printf '  waiting (%ds elapsed): %s\n' "$elapsed" "${verdict#WAIT: }"
    sleep "$interval_s"
  done
}
