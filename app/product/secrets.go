package product

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// secretBox encrypts what the server keeps for users and must not read back
// in clear from a database dump: connector tokens. AES-256-GCM, key derived
// from Options.SecretKey; the nonce prefixes the ciphertext.
type secretBox struct{ aead cipher.AEAD }

func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) == 0 {
		return nil, errors.New("no secret key")
	}
	sum := sha256.Sum256(append([]byte("harness-connectors:"), key...))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead}, nil
}

func (b *secretBox) seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plain, nil)
}

func (b *secretBox) open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed data too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
