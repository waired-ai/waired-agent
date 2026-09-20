//go:build darwin

package runtime

// groupMembers is not needed on darwin: treeAliveFrom answers from the
// kill(-pgid, 0) probe alone there, because launchd reaps orphaned
// processes at once and no container runs waired-agent as PID 1 on macOS.
// It returns no members and no error.
func groupMembers(int) ([]treeMember, error) { return nil, nil }
