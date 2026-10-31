package auth

import "context"

// Static is an in-memory Lookup, for tests and examples.
type Static map[string]*Key

// GetKey implements Lookup.
func (s Static) GetKey(_ context.Context, id string) (*Key, error) {
	if k, ok := s[id]; ok {
		cp := *k
		return &cp, nil
	}
	return nil, ErrNotFound
}

// NewStaticKey generates a key and adds it to s, returning the plaintext.
func (s Static) NewStaticKey(k Key) (string, *Key) {
	g, _ := Generate()
	k.ID, k.SecretHash = g.ID, g.Hash
	if k.SoftBudgetRatio == 0 {
		k.SoftBudgetRatio = 0.8
	}
	s[g.ID] = &k
	return g.Plaintext, s[g.ID]
}
