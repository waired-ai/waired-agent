#!/bin/sh
# install-desktop.sh — install Waired on a Linux desktop.
#
# Run from the extracted dist/native/ tarball as root:
#   sudo ./bin/install-desktop.sh
#
# Lays out:
#   /usr/local/bin/waired               (CLI)
#   /usr/local/bin/waired-agent         (system daemon)
#   /usr/local/bin/waired-tray          (per-user desktop tray)
#   /etc/xdg/autostart/waired-tray.desktop
#   /usr/share/polkit-1/actions/com.waired.policy
#
# Then runs `waired-agent install` which:
#   - creates the `waired` system user
#   - creates /var/lib/waired with restrictive permissions
#   - writes /etc/systemd/system/waired-agent.service (generated, not
#     a tarball copy — the binary owns the unit's content)
#   - writes /etc/waired/agent.env stub if missing
#   - systemctl daemon-reload && systemctl enable waired-agent
#
# After the script runs, the operator must:
#   - export WAIRED_CONTROL_URL=<your-cp-url> (or write to /etc/waired/agent.env)
#   - systemctl start waired-agent
#   - log out & back in (so the autostart fires) and complete `Log in...` from the tray.
set -eu

[ "$(id -u)" -eq 0 ] || { echo "install-desktop.sh: must run as root" >&2; exit 1; }

here="$(cd "$(dirname "$0")/.." && pwd)"

# 1. Stage the binaries.
install -m 0755 "$here/bin/waired"        /usr/local/bin/waired
install -m 0755 "$here/bin/waired-agent"  /usr/local/bin/waired-agent
install -m 0755 "$here/bin/waired-tray"   /usr/local/bin/waired-tray

# 2. Desktop-specific integration that the agent binary does not own.
#    The autostart entry starts the tray on login; the app-launcher in
#    /usr/share/applications makes it discoverable / pinnable in the
#    GNOME application grid so it can be started manually (issue #492).
install -d -m 0755 /etc/xdg/autostart /usr/share/applications /usr/share/polkit-1/actions
install -m 0644 "$here/autostart/waired-tray.desktop"     /etc/xdg/autostart/waired-tray.desktop
install -m 0644 "$here/applications/waired-tray.desktop"  /usr/share/applications/waired-tray.desktop
install -m 0644 "$here/polkit/com.waired.policy"          /usr/share/polkit-1/actions/com.waired.policy

# 3. Let the agent self-register with systemd. This creates the
#    `waired` system user, lays out /var/lib/waired with the right
#    perms, drops the unit at /etc/systemd/system/waired-agent.service,
#    and runs daemon-reload + enable. See internal/platform/service.
/usr/local/bin/waired-agent install \
  --user=waired \
  --state-dir=/var/lib/waired

cat <<EOF
install-desktop.sh: done.

Next steps:
  1. echo "WAIRED_CONTROL_URL=https://your-cp.example.com" >> /etc/waired/agent.env
  2. sudo systemctl start waired-agent      (the service is already enabled)
  3. Log out and back in to start waired-tray (or run 'waired-tray &' from your desktop session).
  4. Right-click the tray icon and pick "Log in..." to enroll this device.
EOF
