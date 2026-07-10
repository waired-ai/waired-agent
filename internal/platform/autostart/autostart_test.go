package autostart

import "testing"

// TestRoundTrip exercises the cross-platform Manager surface. On
// Linux + Windows it uses the platform-specific NewForTest /
// XDG_CONFIG_HOME redirect (see _test_linux.go / _test_windows.go)
// so the real autostart slot is untouched.
func TestRoundTrip(t *testing.T) {
	m := newTestManager(t)
	enabled, err := m.IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled initial: %v", err)
	}
	if enabled {
		t.Fatalf("IsEnabled initial = true; want false")
	}

	if err := m.Enable("C:\\Program Files\\Waired\\waired-tray.exe", []string{"-mgmt", "http://127.0.0.1:9476"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	enabled, err = m.IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled after Enable: %v", err)
	}
	if !enabled {
		t.Fatalf("IsEnabled after Enable = false; want true")
	}

	if err := m.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	enabled, err = m.IsEnabled()
	if err != nil {
		t.Fatalf("IsEnabled after Disable: %v", err)
	}
	if enabled {
		t.Fatalf("IsEnabled after Disable = true; want false")
	}
}
