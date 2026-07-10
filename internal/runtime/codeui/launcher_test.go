package codeui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeInfo_URL(t *testing.T) {
	tok := &RuntimeInfo{ProxyAddr: "127.0.0.1:9480", Auth: AuthToken, Token: "abc123"}
	if got := tok.URL(); got != "http://127.0.0.1:9480/?wt=abc123" {
		t.Errorf("token URL = %q", got)
	}
	basic := &RuntimeInfo{ProxyAddr: "10.0.0.2:9480", Auth: AuthBasic, BasicUser: "waired", BasicPass: "x"}
	if got := basic.URL(); got != "http://10.0.0.2:9480/" {
		t.Errorf("basic URL = %q (must not leak creds in the URL)", got)
	}
}

func TestRuntimeFile_Roundtrip0600(t *testing.T) {
	dir := t.TempDir()
	in := &RuntimeInfo{
		PID: 1234, ProxyAddr: "127.0.0.1:9480", BackendPort: 40000,
		Project: "/home/u/p", Bind: BindLoopback, Auth: AuthToken, Token: "tok",
		StartedUnix: 100, Version: OpenCodePinnedVersion,
	}
	if err := writeRuntime(dir, in); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(runtimePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("runtime.json perm = %o, want 0600 (it holds the capability token)", fi.Mode().Perm())
		}
	}
	out, ok := readRuntime(dir)
	if !ok {
		t.Fatal("readRuntime: not found")
	}
	if out.Token != "tok" || out.ProxyAddr != in.ProxyAddr || out.Project != in.Project {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestReadRuntime_Missing(t *testing.T) {
	if _, ok := readRuntime(t.TempDir()); ok {
		t.Error("expected not-found for empty dir")
	}
}

func TestBuildAuthenticator(t *testing.T) {
	info := &RuntimeInfo{}
	a, err := buildAuthenticator(AuthToken, info)
	if err != nil || a.Mode() != "token" || info.Token == "" {
		t.Fatalf("token auth: a=%v err=%v token=%q", a, err, info.Token)
	}

	t.Setenv(EnvBasicPassword, "") // ensure generation path
	info2 := &RuntimeInfo{}
	b, err := buildAuthenticator(AuthBasic, info2)
	if err != nil || b.Mode() != "basic" || info2.BasicUser != defaultBasicUser || info2.BasicPass == "" {
		t.Fatalf("basic auth: b=%v err=%v user=%q pass=%q", b, err, info2.BasicUser, info2.BasicPass)
	}

	if _, err := buildAuthenticator("nope", &RuntimeInfo{}); err == nil {
		t.Error("unknown auth mode must error")
	}
}

func TestBuildAuthenticator_BasicEnvOverride(t *testing.T) {
	t.Setenv(EnvBasicUser, "alice")
	t.Setenv(EnvBasicPassword, "swordfish")
	info := &RuntimeInfo{}
	if _, err := buildAuthenticator(AuthBasic, info); err != nil {
		t.Fatal(err)
	}
	if info.BasicUser != "alice" || info.BasicPass != "swordfish" {
		t.Errorf("env override not honored: %q/%q", info.BasicUser, info.BasicPass)
	}
}

func TestInstanceMatches(t *testing.T) {
	base := Options{Project: "/home/u/p", Bind: BindLoopback, Auth: AuthToken}
	info := &RuntimeInfo{Project: "/home/u/p", Bind: BindLoopback, Auth: AuthToken}
	if !instanceMatches(info, base) {
		t.Error("identical project/bind/auth should match")
	}
	if instanceMatches(&RuntimeInfo{Project: "/other", Bind: BindLoopback, Auth: AuthToken}, base) {
		t.Error("different project must not match")
	}
	if instanceMatches(&RuntimeInfo{Project: "/home/u/p", Bind: BindOverlay, Auth: AuthToken}, base) {
		t.Error("different bind must not match")
	}
}

func TestResolveBindHost(t *testing.T) {
	if h, err := resolveBindHost(Options{Bind: BindLoopback}); err != nil || h != "127.0.0.1" {
		t.Errorf("loopback => %q, %v", h, err)
	}
	if h, err := resolveBindHost(Options{Bind: "192.168.1.5"}); err != nil || h != "192.168.1.5" {
		t.Errorf("explicit host => %q, %v", h, err)
	}
	// overlay against an unreachable mgmt API must error, not hang.
	if _, err := resolveBindHost(Options{Bind: BindOverlay, MgmtBaseURL: "http://127.0.0.1:1"}); err == nil {
		t.Error("overlay discovery against a dead mgmt API should error")
	}
}

func TestDefaultBaseDir_UnderUserState(t *testing.T) {
	d := DefaultBaseDir()
	if !strings.HasSuffix(filepath.ToSlash(d), "runtimes/codeui") {
		t.Errorf("DefaultBaseDir = %q, want .../runtimes/codeui", d)
	}
}
