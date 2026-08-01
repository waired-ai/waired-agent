package browser

import (
	"reflect"
	"strings"
	"testing"
)

// The package had no tests at all before #181/#182/#183 — three per-OS defects
// in argv nothing could see. These run on whatever OS `go test` runs (CI: Linux
// only) and cover all three, because every builder is untagged and pure.

func TestElevatedFor(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		euid     int
		sudoUser string
		want     string
		wantOK   bool
	}{
		{"linux sudo", "linux", 0, "alice", "alice", true},
		{"darwin sudo", "darwin", 0, "alice", "alice", true},
		{"windows has no sudo concept", "windows", 0, "alice", "", false},
		{"unprivileged linux", "linux", 1000, "alice", "", false},
		{"unprivileged darwin", "darwin", 501, "alice", "", false},
		{"windows euid is -1", "windows", -1, "", "", false},
		{"real root login, no SUDO_USER", "linux", 0, "", "", false},
		{"sudo from root is not a hop", "darwin", 0, "root", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := elevatedFor(tc.goos, tc.euid, tc.sudoUser)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("elevatedFor(%q, %d, %q) = (%q, %v), want (%q, %v)",
					tc.goos, tc.euid, tc.sudoUser, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https", "https://cp.example/login/abc", false},
		{"loopback http (data plane)", "http://127.0.0.1:9479/?token=x", false},
		{"empty", "", true},
		{"leading dash would be parsed as a flag", "--version", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateURL(%q) = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

// TestDarwinHopArgv pins #182: `launchctl asuser` alone still runs as root, so
// the inner `sudo -u` must be there or LaunchServices keeps resolving Safari.
func TestDarwinHopArgv(t *testing.T) {
	got := darwinHopArgv("alice", "501", "https://cp.example/login/abc")
	want := []string{
		"/bin/launchctl", "asuser", "501",
		"/usr/bin/sudo", "-n", "-u", "alice",
		"/usr/bin/open", "https://cp.example/login/abc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("darwinHopArgv:\n got %q\nwant %q", got, want)
	}
}

// TestLinuxHopArgv pins #183: the two session variables sudo strips have to be
// put back, or xdg-open runs against root's MIME database with no session bus.
func TestLinuxHopArgv(t *testing.T) {
	const url = "https://cp.example/login/abc"
	sessionEnv := []string{
		"env",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
	}
	cases := []struct {
		name      string
		bin, kind string
		uid       string
		want      []string
	}{
		{
			name: "runuser is preferred",
			bin:  "/usr/sbin/runuser", kind: hopRunuser, uid: "1000",
			want: append(append([]string{"/usr/sbin/runuser", "-u", "alice", "--"},
				sessionEnv...), "xdg-open", url),
		},
		{
			name: "sudo fallback keeps -H",
			bin:  "/usr/bin/sudo", kind: hopSudo, uid: "1000",
			want: append(append([]string{"/usr/bin/sudo", "-u", "alice", "-H", "--"},
				sessionEnv...), "xdg-open", url),
		},
		{
			name: "unresolved uid still hops, without the session env",
			bin:  "/usr/sbin/runuser", kind: hopRunuser, uid: "",
			want: []string{"/usr/sbin/runuser", "-u", "alice", "--", "xdg-open", url},
		},
		{
			name: "unknown hop kind builds nothing",
			bin:  "/usr/sbin/runuser", kind: "doas", uid: "1000",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := linuxHopArgv(tc.bin, tc.kind, "alice", tc.uid, url)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("linuxHopArgv:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestWindowsRundllCmd pins #181: CreateProcess does not search %PATH% for a
// non-NULL lpApplicationName, so the app name must be absolute (or absent).
func TestWindowsRundllCmd(t *testing.T) {
	const url = "https://cp.example/login/abc"
	const wantCmdline = `rundll32.exe url.dll,FileProtocolHandler ` + url

	cases := []struct {
		name      string
		systemDir string
		wantApp   string
	}{
		{"resolved system directory", `C:\Windows\System32`, `C:\Windows\System32\rundll32.exe`},
		{"trailing backslash is not doubled", `C:\Windows\System32\`, `C:\Windows\System32\rundll32.exe`},
		{"drive root", `C:\`, `C:\rundll32.exe`},
		{"unresolvable system directory falls back to a NULL app name", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, cmdline := windowsRundllCmd(tc.systemDir, url)
			if app != tc.wantApp {
				t.Errorf("windowsRundllCmd(%q) app = %q, want %q", tc.systemDir, app, tc.wantApp)
			}
			if cmdline != wantCmdline {
				t.Errorf("windowsRundllCmd(%q) cmdline = %q, want %q", tc.systemDir, cmdline, wantCmdline)
			}
			if app != "" && !strings.HasSuffix(app, `\rundll32.exe`) {
				t.Errorf("app name %q is not an absolute rundll32 path", app)
			}
		})
	}
}

func TestParseConsoleOwner(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantName string
		wantUID  string
		wantOK   bool
	}{
		{"logged-in console user", "501 alice\n", "alice", "501", true},
		{"login window (root owns the console)", "0 root\n", "", "", false},
		{"non-numeric uid", "nope alice\n", "", "", false},
		{"short output", "501\n", "", "", false},
		{"empty output", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, uid, ok := parseConsoleOwner(tc.out)
			if name != tc.wantName || uid != tc.wantUID || ok != tc.wantOK {
				t.Errorf("parseConsoleOwner(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.out, name, uid, ok, tc.wantName, tc.wantUID, tc.wantOK)
			}
		})
	}
}

func TestScrubEnv(t *testing.T) {
	got := scrubEnv([]string{
		"HOME=/root",
		"WAIRED_STATE_DIR=/var/lib/waired",
		"XDG_CONFIG_HOME=/root/.config",
		"DISPLAY=:0",
		"XAUTHORITY=/home/alice/.Xauthority",
		"LANG=en_US.UTF-8",
	})
	want := []string{"DISPLAY=:0", "XAUTHORITY=/home/alice/.Xauthority", "LANG=en_US.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scrubEnv:\n got %q\nwant %q", got, want)
	}
}

// TestOpenRejectsBadURL exercises the real per-OS Open far enough to prove the
// guard runs before anything is spawned. Whichever OS the suite runs on.
func TestOpenRejectsBadURL(t *testing.T) {
	for _, url := range []string{"", "--version"} {
		if err := Open(url); err == nil {
			t.Errorf("Open(%q) = nil, want an error", url)
		}
	}
}
