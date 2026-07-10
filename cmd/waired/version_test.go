package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
)

func TestWriteVersionHuman(t *testing.T) {
	defer restoreBuildinfo(buildinfo.Version, buildinfo.BuildSHA)
	buildinfo.Version = "0.0.1-rc6"
	buildinfo.BuildSHA = "a1b2c3d"

	var buf bytes.Buffer
	if err := writeVersion(&buf, false); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}
	if got, want := buf.String(), "waired 0.0.1-rc6 (a1b2c3d)\n"; got != want {
		t.Errorf("human output = %q, want %q", got, want)
	}
}

func TestWriteVersionHumanNoSHA(t *testing.T) {
	defer restoreBuildinfo(buildinfo.Version, buildinfo.BuildSHA)
	buildinfo.Version = "0.0.0-dev"
	buildinfo.BuildSHA = ""

	var buf bytes.Buffer
	if err := writeVersion(&buf, false); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}
	if got, want := buf.String(), "waired 0.0.0-dev\n"; got != want {
		t.Errorf("human output = %q, want %q", got, want)
	}
}

func TestWriteVersionJSON(t *testing.T) {
	defer restoreBuildinfo(buildinfo.Version, buildinfo.BuildSHA)
	buildinfo.Version = "1.2.3"
	buildinfo.BuildSHA = "deadbee"

	var buf bytes.Buffer
	if err := writeVersion(&buf, true); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (raw %q)", err, buf.String())
	}
	want := map[string]string{
		"version":  "1.2.3",
		"buildSHA": "deadbee",
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("json[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func restoreBuildinfo(version, sha string) {
	buildinfo.Version = version
	buildinfo.BuildSHA = sha
}
