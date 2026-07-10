package hostsfile

import (
	"reflect"
	"testing"
)

// TestFlushDNSCommands asserts macOS flushes both the directory-services cache
// and mDNSResponder, in that order, without shelling out to the real resolver.
func TestFlushDNSCommands(t *testing.T) {
	var got [][]string
	orig := runFlushCmd
	runFlushCmd = func(name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	defer func() { runFlushCmd = orig }()

	flushDNS()

	want := [][]string{
		{"dscacheutil", "-flushcache"},
		{"killall", "-HUP", "mDNSResponder"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flushDNS commands = %v, want %v", got, want)
	}
}
