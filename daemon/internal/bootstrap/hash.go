package bootstrap

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"hash"
)

// newHash returns a hash for the given algo ("sha512" or, for anything else,
// "sha1"), matching the algorithms the registries expose.
func newHash(algo string) hash.Hash {
	if algo == "sha512" {
		return sha512.New()
	}
	return sha1.New()
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
