// Package id generates the identifiers used across JarJar: ULIDs (lowercase
// Crockford base32, 26 chars) with typed prefixes, plus invite codes and
// secret tokens.
package id

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

var mu sync.Mutex

// ulid builds a 26-char Crockford base32 ULID: 48-bit millisecond timestamp
// followed by 80 bits of randomness. Lowercase per DATA-CONTRACTS.md §1.
func ulid() string {
	mu.Lock()
	defer mu.Unlock()

	ms := uint64(time.Now().UnixMilli())
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("id: crypto/rand failed: " + err.Error())
	}

	// 128-bit value: high 48 bits timestamp, low 80 bits random.
	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], rnd[:])

	return encode32(b)
}

// encode32 encodes 16 bytes (128 bits) as 26 Crockford base32 chars.
func encode32(b [16]byte) string {
	var out [26]byte
	// Process 130 bits (26*5); the top 2 bits are zero-padding.
	var bits uint = 0
	var acc uint64 = 0
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&0x1f]
			acc >>= 5
			bits -= 5
			idx--
		}
	}
	if bits > 0 && idx >= 0 {
		out[idx] = crockford[acc&0x1f]
		idx--
	}
	for idx >= 0 {
		out[idx] = crockford[0]
		idx--
	}
	return string(out[:])
}

// NewRequest returns a request id: req_<ulid>.
func NewRequest() string { return "req_" + ulid() }

// NewPlayer returns a player id: plr_<ulid>.
func NewPlayer() string { return "plr_" + ulid() }

// NewInviteCode returns an 8-char uppercase Crockford invite code.
func NewInviteCode() string {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("id: crypto/rand failed: " + err.Error())
	}
	var b [16]byte
	copy(b[11:], raw[:]) // low 40 bits
	full := encode32(b)
	return strings.ToUpper(full[len(full)-8:])
}

// NewToken returns a URL-safe bearer token (32 random bytes → 43 base64url chars).
func NewToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("id: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// ValidPlayerName reports whether name matches [A-Za-z0-9_-]{1,32}.
func ValidPlayerName(name string) bool {
	if len(name) < 1 || len(name) > 32 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidInviteCode reports whether code is 8 uppercase Crockford chars.
func ValidInviteCode(code string) bool {
	if len(code) != 8 {
		return false
	}
	for _, r := range code {
		if !strings.ContainsRune(strings.ToUpper(crockford), r) {
			return false
		}
	}
	return true
}

// MustParsePrefixed returns an error if s does not start with prefix.
func MustParsePrefixed(s, prefix string) error {
	if !strings.HasPrefix(s, prefix) {
		return fmt.Errorf("id %q missing prefix %q", s, prefix)
	}
	return nil
}
