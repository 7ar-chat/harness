// Package websearch connects the Tavily web search MCP (tools tavily_search and
// scrape_webpage) to Work Chat runs, with the run user's access token — parity
// with Work, where AstraForge mounts WEB_SEARCH_MCP_URL with the user's Studio
// token (ALTO_STUDIO_TOKEN). The api captures the token (legacy: the user's own
// token; token_exchange: an RFC 8693 token for the configured audience) and
// stores it encrypted for the run; the runtime connects the MCP per run with
// `Authorization: Bearer <token>`; the token is deleted when the run ends.
package websearch

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Sealer encrypts run tokens at rest with AES-256-GCM. The ciphertext is bound
// (additional data) to the run id and the user login: a row copied to another
// run or user does not decrypt.
type Sealer struct {
	aead cipher.AEAD
}

// ParseKey decodes WORK_TOKEN_ENCRYPTION_KEY: 32 bytes as base64 (standard or
// URL alphabet, padded or not) or 64 hex characters.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("WORK_TOKEN_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("WORK_TOKEN_ENCRYPTION_KEY must be 32 bytes in base64 (openssl rand -base64 32) or hex")
}

// NewSealer builds a sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("token encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// DevSealer is the development fallback when no key is configured (refused in
// production by config.WebSearchStatus). Every process derives the same key, so
// api and runtime interoperate; it protects nothing.
func DevSealer() *Sealer {
	key := sha256.Sum256([]byte("work-chat-dev-only-token-encryption-key"))
	s, _ := NewSealer(key[:])
	return s
}

func additionalData(runID, login string) []byte {
	return []byte("work-run-credential\x00" + runID + "\x00" + login)
}

// Seal returns nonce || ciphertext of plain, bound to the run and the login.
func (s *Sealer) Seal(plain []byte, runID, login string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, additionalData(runID, login)), nil
}

// ErrUnsealable means a sealed token cannot be decrypted (other key, run or user, or tampered).
var ErrUnsealable = errors.New("sealed token cannot be decrypted")

// Open decrypts a Seal output for the same run and login.
func (s *Sealer) Open(sealed []byte, runID, login string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, ErrUnsealable
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], additionalData(runID, login))
	if err != nil {
		return nil, ErrUnsealable
	}
	return plain, nil
}
