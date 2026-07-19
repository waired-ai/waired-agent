package singleinstance

import "testing"

// TestAcquire_SecondInstanceFails exercises the core guarantee on every
// OS: while one instance holds the guard a second Acquire of the same
// name reports ok=false, and once the first releases, the name is
// re-acquirable. newTestGuard is supplied per-OS (Windows: a unique mutex
// name; Unix: a temp-dir lock path) so the test never touches a real
// waired-tray guard.
func TestAcquire_SecondInstanceFails(t *testing.T) {
	guard := newTestGuard(t)

	release1, ok1, err1 := guard()
	if err1 != nil {
		t.Fatalf("first acquire: unexpected err: %v", err1)
	}
	if !ok1 {
		t.Fatal("first acquire: ok=false, want true (nothing else holds the guard)")
	}

	_, ok2, err2 := guard()
	if err2 != nil {
		t.Fatalf("second acquire: unexpected err: %v", err2)
	}
	if ok2 {
		t.Fatal("second acquire: ok=true, want false (first instance still holds the guard)")
	}

	release1()

	release3, ok3, err3 := guard()
	if err3 != nil {
		t.Fatalf("third acquire: unexpected err: %v", err3)
	}
	if !ok3 {
		t.Fatal("third acquire: ok=false, want true (first instance released)")
	}
	release3()
}

// TestAcquire_ReleaseAlwaysNonNil documents that release is safe to defer
// in every outcome, including ok=false.
func TestAcquire_ReleaseAlwaysNonNil(t *testing.T) {
	guard := newTestGuard(t)

	release1, ok1, _ := guard()
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	defer release1()

	release2, ok2, _ := guard()
	if ok2 {
		t.Fatal("second acquire should fail while first held")
	}
	if release2 == nil {
		t.Fatal("release must be non-nil even when ok=false")
	}
	release2() // must not panic
}
