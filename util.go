package main

import (
	"crypto/rand"
	"encoding/hex"
)

// nonce returns a random token used to deliberately bust the engine cache for
// steps that must never be replayed from cache (credential exchange and side-effecting tasks).
func nonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
