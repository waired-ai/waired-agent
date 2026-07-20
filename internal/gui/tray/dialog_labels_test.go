package tray

// Compile-time cross-OS signature assertion: every OS build of
// ConfirmWithLabels must satisfy this exact shape, so PR2's consent flow
// (and the confirmWithLabels seam) can call it uniformly. No build tag —
// this check runs on whichever GOOS the test compiles for.
var _ func(string, string, string, string) (bool, bool) = ConfirmWithLabels
