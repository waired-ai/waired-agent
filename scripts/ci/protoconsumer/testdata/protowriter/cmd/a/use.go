package main

import "example/proto/p"

func use() string {
	w := p.W{ByLiteral: "set"}
	return w.ByLiteral + w.ByAssign
}
