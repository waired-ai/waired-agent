//go:build !darwin

package keychain

import (
	"errors"
	"testing"
)

func TestStubReturnsUnsupported(t *testing.T) {
	store := New()
	if err := store.Set(Item{Account: "a", Service: "s"}, []byte("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Set: got %v, want ErrUnsupported", err)
	}
	if _, err := store.Get(Item{Account: "a", Service: "s"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Get: got %v, want ErrUnsupported", err)
	}
	if err := store.Delete(Item{Account: "a", Service: "s"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Delete: got %v, want ErrUnsupported", err)
	}
	if _, err := store.Exists(Item{Account: "a", Service: "s"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Exists: got %v, want ErrUnsupported", err)
	}
}
