//go:build !linux && !darwin && !windows

package servicediag

import "context"

// Check has nothing to read on an OS with no service backend: there is no
// registered service to have failed. The zero Result is Unknown, which
// `waired doctor` renders as no finding at all.
func Check(context.Context, string) Result { return Result{} }
