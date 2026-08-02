#!/usr/bin/env python3
"""canary-cache-schema.py — assert the on-disk shape of Claude Code's
gateway-models.json (#407).

waired writes this file itself: Claude Code's discovery fetch is gated on a
credential the agent deliberately never holds (#332/#488), while the /model
picker reads whatever cache already exists. That turns an undocumented private
cache file into a contract waired depends on, so the canary pins it against the
file the REAL client just wrote.

What is asserted is only what the agent-side writer must produce
(internal/integration/claudecode):

  {"baseUrl": str, "fetchedAt": number, "models": [{"id": str,
                                                    "display_name": str?}, ...]}

"fetchedAt" is epoch MILLISECONDS (Date.now()), measured against 2.1.220 —
not an RFC3339 string. The read side schema-parses the whole document, so a
writer that emits the wrong type there does not degrade gracefully: the parse
yields null and the picker silently falls back to the built-in model list,
which is indistinguishable from the bug #407 exists to fix.

Deliberately NOT asserted: the absence of extra fields. The reader strips
unknown keys, so upstream adding one is not drift — it is only drift when a
field waired writes disappears, changes type, or stops matching (baseUrl is
compared by exact string on the read side, so it gets an equality check).

usage: canary-cache-schema.py <gateway-models.json> <expected-base-url>
exit:  0 = shape intact, 1 = drift (reasons on stderr), 2 = usage/read error
"""
import json
import sys


def fail(msg):
    print("  schema: {}".format(msg), file=sys.stderr)


def main():
    if len(sys.argv) != 3:
        print(__doc__.strip().splitlines()[-2], file=sys.stderr)
        return 2
    path, want_base = sys.argv[1], sys.argv[2]

    try:
        with open(path, encoding="utf-8") as f:
            doc = json.load(f)
    except (OSError, ValueError) as e:
        fail("cannot read/parse {}: {}".format(path, e))
        return 2

    bad = 0
    if not isinstance(doc, dict):
        fail("top level is {}, want object".format(type(doc).__name__))
        return 1

    base = doc.get("baseUrl")
    if not isinstance(base, str):
        fail('"baseUrl" is {}, want string'.format(type(base).__name__))
        bad = 1
    elif base != want_base:
        # The read side compares this by exact string, so a normalisation change
        # (trailing slash, scheme rewrite) silently disables the whole cache.
        fail('"baseUrl" is {!r}, want exactly {!r}'.format(base, want_base))
        bad = 1

    # Epoch milliseconds. bool is an int subclass in Python, so exclude it
    # explicitly or `"fetchedAt": true` would pass.
    fetched = doc.get("fetchedAt")
    if isinstance(fetched, bool) or not isinstance(fetched, (int, float)):
        fail('"fetchedAt" is {}, want number (epoch ms)'.format(type(fetched).__name__))
        bad = 1

    models = doc.get("models")
    if not isinstance(models, list):
        fail('"models" is {}, want array'.format(type(models).__name__))
        return 1
    if not models:
        fail('"models" is empty — the client wrote a cache with nothing in it')
        bad = 1

    for i, m in enumerate(models):
        if not isinstance(m, dict):
            fail("models[{}] is {}, want object".format(i, type(m).__name__))
            bad = 1
            continue
        if not isinstance(m.get("id"), str):
            fail('models[{}]["id"] is {}, want string'.format(i, type(m.get("id")).__name__))
            bad = 1
        if "display_name" in m and not isinstance(m["display_name"], str):
            fail('models[{}]["display_name"] is {}, want string'.format(
                i, type(m["display_name"]).__name__))
            bad = 1

    extra = sorted(set(doc) - {"baseUrl", "fetchedAt", "models"})
    if extra:
        # Informational: the reader strips unknown keys, so waired does not need
        # to mirror these. Worth seeing in the log when one appears.
        print("      schema: new top-level field(s) present, not required of the "
              "writer: {}".format(", ".join(extra)))

    if bad:
        return 1
    print("      schema: baseUrl/fetchedAt(number)/models[].id intact ({} models)".format(len(models)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
