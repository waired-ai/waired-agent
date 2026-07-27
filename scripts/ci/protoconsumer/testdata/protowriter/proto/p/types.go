// Fixture for the producedInProto category: the package writes its own
// field, the way proto/hostfit computes a Verdict and the disco codec
// fills a parsed header. Nothing under cmd/ touches ByAssign.
package p

type W struct {
	ByLiteral string
	ByAssign  string
}

func New(s string) W {
	w := W{}
	w.ByAssign = s
	return w
}
