package providerlink

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// pkcePair returns a PKCE verifier and its S256 challenge (RFC 7636).
func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}
