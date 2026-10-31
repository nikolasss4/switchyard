package auth

import (
	"strings"
	"testing"
)

func TestGenerateParseVerify(t *testing.T) {
	g, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Plaintext, "sy_"+g.ID+"_") || len(g.Plaintext) != 3+idLen+1+secretLen {
		t.Fatalf("bad key %q", g.Plaintext)
	}
	id, secret, err := Parse(g.Plaintext)
	if err != nil || id != g.ID {
		t.Fatalf("parse: %q %v", id, err)
	}
	if !Verify(secret, g.Hash) {
		t.Fatal("verify failed for the right secret")
	}
	if Verify(secret[:31]+"x", g.Hash) {
		t.Fatal("verify passed for a wrong secret")
	}
	if strings.Contains(string(g.Hash), secret) {
		t.Fatal("hash must not contain the secret")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, k := range []string{"", "sk-ant-xyz", "sy_short_secret", "sy_abcdefgh", "sy_abcdefgh_" + strings.Repeat("a", 31)} {
		if _, _, err := Parse(k); err == nil {
			t.Errorf("Parse(%q) should fail", k)
		}
	}
}

func TestKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		g, _ := Generate()
		if seen[g.ID] {
			t.Fatal("duplicate id")
		}
		seen[g.ID] = true
	}
}
