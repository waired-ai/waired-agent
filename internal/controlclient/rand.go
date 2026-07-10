package controlclient

import "crypto/rand"

// cryptoRandRead exists so init.go can route through readRandom (a var)
// without exposing the crypto/rand import to tests that want to swap it.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }
