package setup

import (
	"net/http"
)

// EnrollHTTPClient is a thin alias around *http.Client so callers can
// pass nil cleanly without import cycles.
type EnrollHTTPClient struct{ HC *http.Client }

func (e EnrollHTTPClient) toStdlib() *http.Client { return e.HC }
