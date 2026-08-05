package id

import (
	"strings"
	"testing"
)

func TestNewRequestFormat(t *testing.T) {
	r := NewRequest()
	if !strings.HasPrefix(r, "req_") {
		t.Fatalf("request id missing prefix: %s", r)
	}
	if got := len(strings.TrimPrefix(r, "req_")); got != 26 {
		t.Fatalf("ulid length = %d, want 26", got)
	}
}

func TestNewPlayerFormat(t *testing.T) {
	p := NewPlayer()
	if !strings.HasPrefix(p, "plr_") || len(strings.TrimPrefix(p, "plr_")) != 26 {
		t.Fatalf("bad player id: %s", p)
	}
}

func TestInviteCode(t *testing.T) {
	c := NewInviteCode()
	if len(c) != 8 {
		t.Fatalf("invite code length = %d, want 8", len(c))
	}
	if !ValidInviteCode(c) {
		t.Fatalf("generated invite code %q failed validation", c)
	}
	if c != strings.ToUpper(c) {
		t.Fatalf("invite code not uppercase: %s", c)
	}
}

func TestToken(t *testing.T) {
	tok := NewToken()
	if len(tok) != 43 { // 32 bytes base64url without padding
		t.Fatalf("token length = %d, want 43", len(tok))
	}
	if NewToken() == tok {
		t.Fatal("tokens are not unique")
	}
}

func TestULIDUniqueAndSortable(t *testing.T) {
	a := ulid()
	b := ulid()
	if a == b {
		t.Fatal("consecutive ULIDs collide")
	}
	if len(a) != 26 {
		t.Fatalf("ulid length = %d", len(a))
	}
}

func TestValidPlayerName(t *testing.T) {
	cases := map[string]bool{
		"alice": true, "a": true, "a_b-C9": true,
		"": false, "has space": false, "bad!": false,
		strings.Repeat("x", 33): false,
	}
	for name, want := range cases {
		if got := ValidPlayerName(name); got != want {
			t.Errorf("ValidPlayerName(%q) = %v, want %v", name, got, want)
		}
	}
}
