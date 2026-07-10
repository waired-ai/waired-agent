//go:build testharness && !linux

package testharness

import (
	"context"
	"errors"
)

// IPTablesChain matches the linux build's constant so cross-platform
// reads of the symbol compile. The non-linux ExecIPTabler still
// returns errNotLinux for any operation.
const IPTablesChain = "WAIRED_TESTHARNESS"

// IPTabler is the same interface as on linux; the non-linux ExecIPTabler
// always returns errNotLinux. Tests that use a recording IPTabler
// continue to compile and run on non-linux, but production paths
// inside the testharness binary (only ever run on linux) cannot
// actually invoke iptables here.
type IPTabler interface {
	Run(ctx context.Context, args ...string) (output string, exitCode int, err error)
}

type ExecIPTabler struct{}

var errNotLinux = errors.New("testharness: iptables ops are linux-only")

func (ExecIPTabler) Run(_ context.Context, _ ...string) (string, int, error) {
	return "", -1, errNotLinux
}

func InstallChain(_ context.Context, _ IPTabler) error { return errNotLinux }

func BlockUDPDirect(_ context.Context, _ IPTabler, _ int, _ []string, _ string) error {
	return errNotLinux
}

func FlushChain(_ context.Context, _ IPTabler) error { return errNotLinux }
