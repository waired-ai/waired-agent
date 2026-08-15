package waired

import "net/http"

// A client in a _test.go file must be invisible to the guard: tests build
// clients to stand up fixtures, which is not the decision being guarded.
func testOnlyClient() *http.Client { return &http.Client{} }
