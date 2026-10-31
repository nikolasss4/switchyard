// Package auth issues and verifies gateway API keys.
//
// A key looks like sy_<id>_<secret>. The id is a public lookup handle; only the
// SHA-256 of the secret is stored. A fast hash is the right tool here: the
// secret is 190 random bits, so there is nothing to brute-force, and a slow
// password hash such as bcrypt would add tens of milliseconds to every request.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"
)

const (
	prefix    = "sy_"
	idLen     = 8
	secretLen = 32
	alphabet  = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// ErrMalformed is returned for strings that are not switchyard keys.
var ErrMalformed = errors.New("malformed API key")

// Generated is a new key. Plaintext is shown to the user once.
type Generated struct {
	ID        string
	Plaintext string
	Hash      []byte
}

// Generate returns a new random key.
func Generate() (Generated, error) {
	id, err := randString(idLen)
	if err != nil {
		return Generated{}, err
	}
	secret, err := randString(secretLen)
	if err != nil {
		return Generated{}, err
	}
	return Generated{ID: id, Plaintext: prefix + id + "_" + secret, Hash: HashSecret(secret)}, nil
}

// Parse splits a key string into its id and secret.
func Parse(key string) (id, secret string, err error) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", "", ErrMalformed
	}
	id, secret, ok = strings.Cut(rest, "_")
	if !ok || len(id) != idLen || len(secret) != secretLen {
		return "", "", ErrMalformed
	}
	return id, secret, nil
}

// HashSecret hashes a key secret for storage.
func HashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// Verify compares a secret with a stored hash in constant time.
func Verify(secret string, hash []byte) bool {
	return subtle.ConstantTimeCompare(HashSecret(secret), hash) == 1
}

func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 256 is not a multiple of 62; reject bytes past the last full cycle to avoid bias.
	out := make([]byte, 0, n)
	for len(out) < n {
		for _, c := range b {
			if c < 248 && len(out) < n {
				out = append(out, alphabet[int(c)%len(alphabet)])
			}
		}
		if len(out) < n {
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
		}
	}
	return string(out), nil
}
