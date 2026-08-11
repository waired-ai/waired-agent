//go:build windows

package console

import (
	"errors"
	"testing"
)

// swapCP installs fake code-page syscalls for one test and records every write.
// The fakes take and return the real arguments, so a call that sets the wrong
// page is a visible failure rather than an invisible one (CLAUDE.md §Test
// discipline).
func swapCP(t *testing.T, start uint32, getErr, setErr error) (cur *uint32, writes *[]uint32) {
	t.Helper()
	page := start
	var seen []uint32
	oldGet, oldSet := getOutputCP, setOutputCP
	getOutputCP = func() (uint32, error) {
		if getErr != nil {
			return 0, getErr
		}
		return page, nil
	}
	setOutputCP = func(cp uint32) error {
		if setErr != nil {
			return setErr
		}
		seen = append(seen, cp)
		page = cp
		return nil
	}
	t.Cleanup(func() { getOutputCP, setOutputCP = oldGet, oldSet })
	return &page, &seen
}

// TestSetOutputUTF8OnCP932 is the reported host: the page moves to CP_UTF8 for
// the run and is put back afterwards.
func TestSetOutputUTF8OnCP932(t *testing.T) {
	page, writes := swapCP(t, 932, nil, nil)

	restore := SetOutputUTF8()
	if *page != CPUTF8 {
		t.Fatalf("during the run the console is on %d, want %d", *page, CPUTF8)
	}
	if !OutputIsUTF8() {
		t.Error("OutputIsUTF8() = false during the run, want true")
	}

	restore()
	if *page != 932 {
		t.Errorf("after restore the console is on %d, want the 932 it started on", *page)
	}
	if want := []uint32{CPUTF8, 932}; len(*writes) != 2 || (*writes)[0] != want[0] || (*writes)[1] != want[1] {
		t.Errorf("code-page writes = %v, want %v", *writes, want)
	}
}

// TestSetOutputUTF8NoConsole covers waired-agent under the SCM and waired-tray:
// no console, so nothing is written and glyphs stay off.
func TestSetOutputUTF8NoConsole(t *testing.T) {
	_, writes := swapCP(t, 0, errors.New("The handle is invalid."), nil)

	restore := SetOutputUTF8()
	restore()

	if len(*writes) != 0 {
		t.Errorf("code-page writes = %v, want none when there is no console", *writes)
	}
	if OutputIsUTF8() {
		t.Error("OutputIsUTF8() = true with no console, want false")
	}
}

// TestSetOutputUTF8AlreadyUTF8 pins that we do not restore a page we did not
// change — otherwise a console the user had already put on CP_UTF8 would be
// written to twice for no reason.
func TestSetOutputUTF8AlreadyUTF8(t *testing.T) {
	_, writes := swapCP(t, CPUTF8, nil, nil)

	restore := SetOutputUTF8()
	restore()

	if len(*writes) != 0 {
		t.Errorf("code-page writes = %v, want none when the console is already UTF-8", *writes)
	}
	if !OutputIsUTF8() {
		t.Error("OutputIsUTF8() = false on a CP_UTF8 console, want true")
	}
}

// TestSetOutputUTF8AgainstTheRealConsole calls the real syscalls, because the
// tests above swap them out and would pass just as happily if getOutputCP and
// setOutputCP were wired to the wrong Win32 entry points (CLAUDE.md §Test
// discipline: a `var xFn = realFn` seam needs a test on realFn).
//
// Both host shapes are asserted rather than skipped: `go test` under CI has its
// stdout redirected but usually still has a console attached, and a runner with
// no console at all must take the no-op path.
func TestSetOutputUTF8AgainstTheRealConsole(t *testing.T) {
	before, err := getOutputCP()
	hasConsole := err == nil

	restore := SetOutputUTF8()
	t.Cleanup(restore)

	if !hasConsole {
		if OutputIsUTF8() {
			t.Error("OutputIsUTF8() = true with no console attached, want false")
		}
		return
	}

	if !OutputIsUTF8() {
		cp, _ := getOutputCP()
		t.Fatalf("console is on code page %d after SetOutputUTF8, want %d", cp, CPUTF8)
	}
	restore()
	after, err := getOutputCP()
	if err != nil {
		t.Fatalf("reading the code page back: %v", err)
	}
	if after != before {
		t.Errorf("console left on code page %d, want the %d it started on", after, before)
	}
}

// TestSetOutputUTF8SetFails is the legacy-console case: the set is refused, the
// console keeps its original page, and OutputIsUTF8 says so — which is what
// makes the caller degrade to ASCII rather than write mojibake.
func TestSetOutputUTF8SetFails(t *testing.T) {
	page, _ := swapCP(t, 932, nil, errors.New("Invalid parameter."))

	restore := SetOutputUTF8()
	if *page != 932 {
		t.Errorf("console moved to %d despite the failed set, want 932", *page)
	}
	if OutputIsUTF8() {
		t.Error("OutputIsUTF8() = true after a failed set, want false")
	}
	restore() // must not panic, and must not write
	if *page != 932 {
		t.Errorf("restore moved the console to %d, want 932", *page)
	}
}
