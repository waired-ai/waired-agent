package agentconfig

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"DEBUG", slog.LevelDebug, false}, // case-insensitive
		{"  debug  ", slog.LevelDebug, false},
		{"warning", slog.LevelInfo, true}, // not accepted; slog uses "warn"
		{"trace", slog.LevelInfo, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLogLevel(tc.in)
			if tc.wantErr && err == nil {
				t.Errorf("ParseLogLevel(%q) = nil error, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ParseLogLevel(%q) = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaults_LogLevelInfo(t *testing.T) {
	cfg := Defaults()
	if cfg.Logging.Level != LogLevelInfo {
		t.Errorf("Defaults().Logging.Level = %q, want %q", cfg.Logging.Level, LogLevelInfo)
	}
	if cfg.Logging.SlogLevel() != slog.LevelInfo {
		t.Errorf("Defaults().Logging.SlogLevel() = %v, want Info", cfg.Logging.SlogLevel())
	}
}

func TestValidate_LogLevel(t *testing.T) {
	cases := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"debug", false},
		{"info", false},
		{"warn", false},
		{"error", false},
		{"verbose", true},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			cfg := Defaults()
			cfg.Logging.Level = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(level=%q) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(level=%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestMergeEnv_LogLevel(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_LOG_LEVEL=DEBUG"}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Logging.Level != LogLevelDebug {
		t.Errorf("Logging.Level = %q, want %q (normalized)", cfg.Logging.Level, LogLevelDebug)
	}
	if cfg.Logging.SlogLevel() != slog.LevelDebug {
		t.Errorf("SlogLevel() = %v, want Debug", cfg.Logging.SlogLevel())
	}
}

func TestMergeEnv_LogLevel_Bad(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_LOG_LEVEL=loud"}); err == nil {
		t.Fatalf("MergeEnv(WAIRED_LOG_LEVEL=loud) = nil, want error")
	}
}

func TestMergeEnv_LogLevel_OverridesJSONLayer(t *testing.T) {
	// Env overlays the JSON layer: a prior logging.level is replaced.
	cfg := Defaults()
	cfg.Logging.Level = LogLevelWarn // stands in for a value merged from agent.json
	if err := cfg.MergeEnv([]string{"WAIRED_LOG_LEVEL=error"}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Logging.Level != LogLevelError {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, LogLevelError)
	}
}
