package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/notice"
)

// updateNoticePublisher is the "update" producer: it says that a newer
// release than the one this computer runs is available.
//
// It needs no machinery of its own. runUpdateCheckLoop already asks the
// network every six hours and caches the answer, and Status is a pure
// read of that cache that "never forc[es] a network hit" (update.go), so
// republishing on the notice cadence costs nothing and the check keeps
// its own, much longer, schedule.
//
// This is the producer that made the notice loop's placement matter: an
// update is worth saying on a computer that runs no models at all, so
// this one is started outside every inference gate, beside the check
// loop it reads from.
func updateNoticePublisher(reg *notice.Registry, uc *updateController) func(context.Context) {
	if reg == nil || uc == nil {
		return nil
	}
	return func(ctx context.Context) {
		st, err := uc.Status(ctx)
		if err != nil {
			// Status does not fail today. If it ever does, saying
			// nothing is right: the lease lapses and the row goes,
			// rather than a stale version sitting there being offered.
			reg.Publish(noticeSourceUpdate, nil)
			return
		}
		reg.Publish(noticeSourceUpdate, updateNotices(st))
	}
}

// updateNotices is the mapping, split from the read above so the rules
// it encodes are testable without an update controller or a network.
//
// A failed check publishes nothing. UpdateStatus.Error means "I could
// not find out", and a notice saying that would be a row a person cannot
// act on about a question they did not ask; `waired update` is where
// someone who wants to know goes, and it prints the error there.
func updateNotices(st management.UpdateStatus) []notice.Notice {
	if !st.Available || st.LatestVersion == "" {
		return nil
	}
	return []notice.Notice{notice.UpdateAvailable(st.CurrentVersion, st.LatestVersion)}
}
