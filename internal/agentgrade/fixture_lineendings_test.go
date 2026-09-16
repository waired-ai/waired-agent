package agentgrade

import (
	"strings"
	"testing"
)

// Product contract (waired-agent#1371): the fixture a model is graded on,
// and the revision the stored verdicts are keyed by, do not depend on the
// line endings of the checkout the probe was built from. A Windows build
// used to grade a CRLF copy under a different revision.
func TestFixtureContextIsTheSameOnEveryCheckout(t *testing.T) {
	if strings.Contains(fixtureProjectContext, "\r") {
		t.Fatal("the session context carries a carriage return; a CRLF checkout would change what the model reads and the fixture revision")
	}
	crlf := strings.ReplaceAll(fixtureProjectContextFile, "\n", "\r\n")
	crlf = strings.ReplaceAll(crlf, "\r\r\n", "\r\n")
	if got := normalizeFixtureLineEndings(crlf); got != fixtureProjectContext {
		t.Error("a CRLF copy of the session context does not normalise to the text the probe sends")
	}
}
