package main

import (
	"errors"
	"io/fs"
)

// identityAction is what the daemon should do about its own identity file
// when it notices the file is not where it left it.
//
// The daemon owns its state directory and holds the identity in memory for
// the whole of its life, so it is the only process that can put the file
// back losslessly. That is the whole of waired-agent#800's first two
// symptoms: `rm -rf <state-dir>` under a running daemon leaves the session
// serving from memory while the CLI reads the disk and reports "Not
// enrolled", and `waired init` then "resumes" into that gap without
// repairing anything.
type identityAction int

const (
	// identityNoAction — the file is there, or there is nothing to write.
	// A file that EXISTS is never rewritten: it may be a different
	// identity someone put there deliberately, and clobbering it would
	// destroy the only copy.
	identityNoAction identityAction = iota
	// identityRestore — the file is absent and this process holds the
	// identity it should contain.
	identityRestore
	// identityReport — something is wrong that writing cannot fix, and
	// that must not be papered over.
	identityReport
)

// decideIdentityRepair is the rule, kept apart from the filesystem so all
// four cases are reachable from a table test on any runner.
//
// Only ABSENCE is repaired. A file that is present is never overwritten,
// and a file that cannot be READ is never treated as absent.
//
// That last clause is the lesson of waired-agent#778 applied one layer
// over: there, an EACCES from os.Stat on a vLLM interpreter produced the
// same answer as "no install", so a complete engine read as missing and
// `waired init` waited forever for it to arrive. Here the same conflation
// would turn a permissions problem — someone tightened the state dir, a
// volume came back read-only — into a write that fails, or worse, into a
// silent "repaired" that fixed nothing. EACCES is a REPORTABLE state, not
// a repairable one.
//
// Record of today's behaviour (waired-agent#800). Daemon self-repair is
// new here; see the PR body for the ruling that scoped it.
func decideIdentityRepair(statErr error, haveIdentity bool) identityAction {
	switch {
	case statErr == nil:
		return identityNoAction
	case errors.Is(statErr, fs.ErrNotExist):
		if !haveIdentity {
			// An unenrolled daemon has no identity file and should not:
			// this is the resting state, not damage.
			return identityNoAction
		}
		return identityRestore
	default:
		return identityReport
	}
}
