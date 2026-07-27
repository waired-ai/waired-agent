package main

import "example/proto/p"

func fill(raw []byte, s string) p.W {
	w := p.W{ByLiteral: "set"}
	w.ByAssign = s
	w.ByOpAssign += 1
	w.ByIncDec++
	take(&w.ByAddress)
	copy(w.BySlice[:], raw)
	w.ByIndex = make([]string, 1)
	w.ByIndex[0] = s
	return w
}

func take(*string) {}
