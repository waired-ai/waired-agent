package waired

import (
	"net/http"
	"time"
)

// Fixture for mgmtclientguard's own tests. Both syntaxes appear, because
// the defect the guard exists for used both: three of the six #785 sites
// built a composite literal and three referenced http.DefaultClient.

func literalClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Second}
}

func defaultClient() *http.Client {
	return http.DefaultClient
}

// A same-named field on another type must not count — the guard keys on
// the http package selector, not on the identifier alone.
type notAClient struct{ Client string }

func decoy() notAClient { return notAClient{Client: "http.Client"} }
