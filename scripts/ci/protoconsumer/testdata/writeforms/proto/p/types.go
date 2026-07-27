// Fixture for the write forms collectWrites has to recognise. Every
// field here is written by cmd/a/writes.go in a different syntactic
// shape; a field the collector cannot see would show up as a violation.
package p

type W struct {
	ByLiteral  string
	ByAssign   string
	ByOpAssign int
	ByIncDec   int
	ByAddress  string
	BySlice    [32]byte
	ByIndex    []string
}
