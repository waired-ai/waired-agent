#!/bin/sh
# capture-app-macos.sh — take the Waired app captures for docs.waired.ai on a Mac.
#
# Produces app-ready.png and app-not-signed-in.png for docs-site/public/img/
# (CAPTURES.md says what each shows and the rules that apply to every capture).
#
# Run it from a Terminal window, as the user who is logged in at the screen:
#
#     sh docs-site/scripts/capture-app-macos.sh          # take both captures
#     sh docs-site/scripts/capture-app-macos.sh check    # only report what it would use
#
# Terminal needs two permissions, granted once in System Settings > Privacy &
# Security: Accessibility, so System Events can open the app's menu, and
# Screen & System Audio Recording, for screencapture. No step asks for a
# password.
#
# Environment (all optional):
#   WD_APP    the app bundle to capture            default /Applications/Waired.app
#   WD_AGENT  a waired-agent of the same build, run unenrolled for the
#             not-signed-in capture               default /usr/local/bin/waired-agent
#   WD_OUT    where the files and the working directories go
#                                                 default ~/waired-captures
#
# What it does, in order:
#   1. Quits the running app and starts WD_APP against the real daemon. Waits
#      for "● Connected" and "● Engine: ready", opens the menu through System
#      Events, captures the menu's own rectangle at the display's scale, and
#      repaints the account row as you@example.com  →  out/app-ready.png
#   2. Starts WD_AGENT unenrolled, on a scratch state directory and its own
#      port, points the app at it through WAIRED_MGMT_SOCKET, waits for
#      "○ Not signed in", captures  →  out/app-not-signed-in.png
#   3. Stops both, relaunches the installed app, and asks the real daemon to
#      start its inference engine again.
#
# Quitting the app stops the engine and suspends sharing (internal/gui/tray,
# onQuit, #186); relaunching resumes sharing, and the engine/start request at
# the end brings the engine back. Unmasked captures stay in raw/ so they can
# be checked against the masked ones; delete them afterwards.
set -u

WD_APP="${WD_APP:-/Applications/Waired.app}"
WD_AGENT="${WD_AGENT:-/usr/local/bin/waired-agent}"
WD_OUT="${WD_OUT:-$HOME/waired-captures}"
TRAY="$WD_APP/Contents/MacOS/waired-tray"
OUT="$WD_OUT/out"; RAW="$WD_OUT/raw"; TMP="$WD_OUT/tmp"; STATE="$WD_OUT/state"
MASK="$(cd "$(dirname "$0")" && pwd)/capture-app-mask.swift"
REAL_SOCK=/var/run/waired/mgmt.sock
SCRATCH_MGMT=127.0.0.1:9486

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

check() {
	echo "app:        $WD_APP"
	[ -x "$TRAY" ] && "$TRAY" -version 2>/dev/null | head -1 || echo "  (no waired-tray binary at $TRAY)"
	echo "agent:      $WD_AGENT"
	[ -x "$WD_AGENT" ] && "$WD_AGENT" -version 2>/dev/null | head -1 || echo "  (no waired-agent binary at $WD_AGENT)"
	echo "output:     $WD_OUT"
	echo "mask:       $MASK"
	echo "terminal:   ${TERM_PROGRAM:-not Terminal.app}"
	echo "appearance: $(defaults read -g AppleInterfaceStyle 2>/dev/null || echo Light) (the docs captures are Dark)"
	echo "daemon:     HTTP $(curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:9476/waired/v1/status) from the real daemon"
	echo "socket:     $([ -S "$REAL_SOCK" ] && echo "$REAL_SOCK" || echo "$REAL_SOCK is missing")"
	echo "python3:    $(command -v python3 || echo missing)"
	echo "swift:      $(xcrun --find swiftc 2>/dev/null || echo missing)"
	if xcrun swiftc -typecheck "$MASK" 2>/dev/null; then echo "mask.swift: typechecks"; else echo "mask.swift: does not typecheck"; fi
}

if [ "${1:-}" = "check" ]; then check; exit 0; fi

mkdir -p "$OUT" "$RAW" "$TMP" "$STATE"
rm -rf "${STATE:?}"/*
COPY_PID=""; AGENT_PID=""

model_field() {  # $1 = field name; reads the app's debug JSON
	python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1]))["model"].get(sys.argv[2],""))
except Exception: print("")' "$TMP/waired-tray-debug.json" "$1" 2>/dev/null
}

wait_for() {  # $1 = field, $2 = prefix, $3 = seconds
	i=0
	while [ "$i" -lt "$3" ]; do
		v=$(model_field "$1")
		case "$v" in "$2"*) echo "$v"; return 0 ;; esac
		sleep 2; i=$((i+2))
	done
	echo "timeout waiting for $1 = $2* (last: $(model_field "$1"))" >&2
	return 1
}

start_app() {  # $@ = extra flags; the caller may set WAIRED_MGMT_SOCKET
	rm -f "$TMP/waired-tray-debug.json"
	WAIRED_TRAY_DEBUG=1 TMPDIR="$TMP/" "$TRAY" -poll-every 2s "$@" >"$TMP/tray.log" 2>&1 &
	COPY_PID=$!
	sleep 3
}

stop_app() {
	[ -n "$COPY_PID" ] && kill "$COPY_PID" 2>/dev/null && wait "$COPY_PID" 2>/dev/null
	COPY_PID=""
	sleep 2
}

# shot NAME opens the menu, writes its rows to tmp/NAME-rows.txt (name,
# enabled, x, y, w, h in points), and captures the menu's rectangle to
# raw/NAME-raw.png. The click on a status item does not return while its menu
# is open (menu tracking is modal), so it runs in a background osascript and a
# second one reads the open menu, captures, and presses Escape.
shot() {
	rm -f "$RAW/$1-raw.png"
	osascript -e 'tell application "System Events" to tell process "Waired" to click menu bar item 1 of menu bar (count of menu bars)' >/dev/null 2>&1 &
	click=$!
	sleep 1.5
	osascript >"$TMP/$1-rows.txt" 2>&1 <<EOF
tell application "System Events"
	tell process "Waired"
		set mbi to menu bar item 1 of menu bar (count of menu bars)
		set m to menu 1 of mbi
		set {mx, mty} to position of m
		set {mw, mh} to size of m
		log "MENU	" & mx & "	" & mty & "	" & mw & "	" & mh
		repeat with mi in menu items of m
			set nm to name of mi
			if nm is missing value then set nm to "-"
			set {ix, iy} to position of mi
			set {iw, ih} to size of mi
			log "ITEM	" & nm & "	" & (enabled of mi) & "	" & ix & "	" & iy & "	" & iw & "	" & ih
		end repeat
		tell me to do shell script "screencapture -x -R " & mx & "," & mty & "," & mw & "," & mh & " '$RAW/$1-raw.png'"
	end tell
	key code 53
end tell
EOF
	sleep 0.5
	kill "$click" 2>/dev/null
	if [ ! -s "$RAW/$1-raw.png" ]; then
		log "capture failed for $1:"; cat "$TMP/$1-rows.txt"; return 1
	fi
	log "captured $1: $(sips -g pixelWidth -g pixelHeight "$RAW/$1-raw.png" | awk '/pixel/ {printf "%s ", $2}')"
}

# The display scale, from the capture's pixel width over the menu's width in points.
scale_of() {  # $1 = rows file, $2 = png
	python3 - "$1" "$2" <<'PY'
import subprocess, sys
mw = next(int(l.split("\t")[3]) for l in open(sys.argv[1], encoding="utf-8") if l.startswith("MENU\t"))
px = int(subprocess.check_output(["sips", "-g", "pixelWidth", sys.argv[2]]).decode().split()[-1])
print(max(1, round(px / mw)))
PY
}

# Rows carrying an e-mail address or a host name become mask specs
# ("yTop,height,replacement", y relative to the menu's top, in points).
mask_specs() {  # $1 = rows file
	python3 - "$1" <<'PY'
import re, sys
top = None
for line in open(sys.argv[1], encoding="utf-8"):
    p = line.rstrip("\n").split("\t")
    if p[0] == "MENU":
        top = int(p[2])
    elif p[0] == "ITEM" and top is not None and len(p) >= 7:
        name = p[1]
        new = re.sub(r"[\w.+-]+@[\w.-]+\.\w+", "you@example.com", name)
        new = re.sub(r"\b(?:pc|sv|mac)-[\w.-]+|\b[\w-]+\.local\b", "my-desktop", new)
        if new != name:
            print(f"{int(p[4]) - top},{p[6]},{new}")
PY
}

printable_rows() {  # the rows file with identifiers hidden, for the log
	sed -E 's/[[:alnum:]._+-]+@[[:alnum:].-]+\.[[:alpha:]]+/<email>/g; s/\b(pc|sv|mac)-[[:alnum:]._-]+/<host>/g' "$1"
}

restore() {
	log "putting the installed app back"
	stop_app
	if [ -n "$AGENT_PID" ]; then kill "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""; fi
	pgrep -x waired-tray >/dev/null || open -a /Applications/Waired.app
	sleep 4
	# The same two requests the app sends on its next start, so the computer
	# is left as found even if the relaunch did not get that far.
	code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST --unix-socket "$REAL_SOCK" http://waired/waired/v1/sharing/unsuspend)
	log "asked the real daemon to resume sharing: HTTP $code"
	code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST --unix-socket "$REAL_SOCK" http://waired/waired/v1/inference/engine/start)
	log "asked the real daemon to start its inference engine again: HTTP $code"
}
trap restore EXIT

case "$(defaults read -g AppleInterfaceStyle 2>/dev/null)" in
	Dark) ;;
	*) log "note: the appearance is Light; the docs captures are Dark" ;;
esac

# --- 1. app-ready.png, against the real daemon -----------------------------
log "quitting the running Waired app"
pkill -x waired-tray   # by process name: a -f pattern would match this script's own shell
sleep 3
start_app -mgmt http://127.0.0.1:9476
curl -s -m 10 -o /dev/null -X POST --unix-socket "$REAL_SOCK" http://waired/waired/v1/inference/engine/start
log "waiting for the engine (up to 4 minutes)"
wait_for HeaderTitle "● Connected" 30 >/dev/null || exit 1
wait_for StatusEngineLabel "● Engine: ready" 240 >/dev/null || exit 1
# After an engine restart the daemon reloads the model it holds resident on
# its own, and the row says "(not loaded)" until then; one request through
# the local gateway hurries it along.
curl -s -m 180 -o /dev/null http://127.0.0.1:9473/v1/chat/completions -H 'Content-Type: application/json' \
	-d '{"model":"waired/default","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' &
i=0
while [ "$i" -lt 120 ]; do
	case "$(model_field StatusEngineLabel)" in *"(not loaded)") sleep 2; i=$((i+2)) ;; *) break ;; esac
done
log "$(model_field StatusEngineLabel)"
shot app-ready || exit 1
printable_rows "$TMP/app-ready-rows.txt"
specs=$(mask_specs "$TMP/app-ready-rows.txt")
scale=$(scale_of "$TMP/app-ready-rows.txt" "$RAW/app-ready-raw.png")
if [ -n "$specs" ]; then
	IFS='
'
	set -f
	# shellcheck disable=SC2086
	swift "$MASK" "$RAW/app-ready-raw.png" "$OUT/app-ready.png" "$scale" $specs || exit 1
	set +f
	unset IFS
else
	cp "$RAW/app-ready-raw.png" "$OUT/app-ready.png"
fi
stop_app

# --- 2. app-not-signed-in.png, against an unenrolled daemon ----------------
log "starting an unenrolled daemon on a scratch state directory"
"$WD_AGENT" --state-dir "$STATE" --mgmt "$SCRATCH_MGMT" --mgmt-socket "$STATE/mgmt.sock" \
	--disable-inference --punch-enabled=false >"$TMP/agent.log" 2>&1 &
AGENT_PID=$!
sleep 4
# The app reads through the management socket only while -mgmt is the
# default authority (internal/gui/tray/mgmt.go, newReadClient), so it keeps
# the default URL and the socket override alone points it at the scratch
# daemon; the scratch daemon's own TCP port is never used.
WAIRED_MGMT_SOCKET="$STATE/mgmt.sock" start_app -mgmt http://127.0.0.1:9476
wait_for HeaderTitle "○ Not signed in" 30 || exit 1
shot app-not-signed-in || exit 1
printable_rows "$TMP/app-not-signed-in-rows.txt"
if [ -n "$(mask_specs "$TMP/app-not-signed-in-rows.txt")" ]; then
	log "the not-signed-in menu carries an identifier; not copying it"; exit 1
fi
cp "$RAW/app-not-signed-in-raw.png" "$OUT/app-not-signed-in.png"
stop_app
kill "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""

log "done: $OUT/app-ready.png and $OUT/app-not-signed-in.png (unmasked copies in $RAW)"
