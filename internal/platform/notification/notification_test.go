package notification

import "testing"

// TestNew_RoundTripStub verifies the constructor produces a non-nil
// Notifier and that the interface is satisfied (catches build-tag
// mistakes where a platform-specific newNotifier is missing). The
// actual rendering is best-effort and can't be asserted in CI; the
// integration check is "did Notify return nil for non-empty title".
func TestNew_RoundTripStub(t *testing.T) {
	n := New()
	if n == nil {
		t.Fatal("New returned nil")
	}
	if err := n.Notify("waired", "test notification", Info); err != nil {
		t.Fatalf("Notify(Info) = %v, want nil", err)
	}
	if err := n.Notify("waired", "warning", Warning); err != nil {
		t.Fatalf("Notify(Warning) = %v, want nil", err)
	}
	if err := n.Notify("waired", "error", Error); err != nil {
		t.Fatalf("Notify(Error) = %v, want nil", err)
	}
}

func TestNotify_EmptyTitleRejected(t *testing.T) {
	n := New()
	if err := n.Notify("", "body", Info); err == nil {
		t.Errorf("Notify(empty title) should error; got nil")
	}
}
