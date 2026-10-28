package websearch_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/GhaziBenDahmane/harness/websearch"
)

func TestSealerRoundTripIsBoundToRunAndUser(t *testing.T) {
	// A sealed token opens only for the same run and login; tampering and another key are rejected.
	// Arrange
	s, _ := websearch.NewSealer(testKey())
	other, _ := websearch.NewSealer(bytes.Repeat([]byte{7}, 32))
	sealed, err := s.Seal([]byte("secret-token"), "run-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.Seal([]byte("secret-token"), "run-1", "alice")
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	// Act
	plain, openErr := s.Open(sealed, "run-1", "alice")
	_, otherRun := s.Open(sealed, "run-2", "alice")
	_, otherUser := s.Open(sealed, "run-1", "bob")
	_, tamperErr := s.Open(tampered, "run-1", "alice")
	_, keyErr := other.Open(sealed, "run-1", "alice")
	_, shortErr := s.Open([]byte{1, 2}, "run-1", "alice")

	// Assert
	if openErr != nil || string(plain) != "secret-token" {
		t.Fatalf("open %q %v", plain, openErr)
	}
	if bytes.Contains(sealed, []byte("secret-token")) || bytes.Equal(sealed, again) {
		t.Fatal("ciphertext leaks the token or reuses a nonce")
	}
	for _, err := range []error{otherRun, otherUser, tamperErr, keyErr, shortErr} {
		if !errors.Is(err, websearch.ErrUnsealable) {
			t.Fatalf("want ErrUnsealable, got %v", err)
		}
	}
}

func TestParseKeyAcceptsBase64AndHexOf32Bytes(t *testing.T) {
	// WORK_TOKEN_ENCRYPTION_KEY is 32 bytes in base64 (any alphabet) or hex; anything else is refused.
	key := testKey()
	cases := []struct {
		name, in string
		ok       bool
	}{
		{"std base64", base64.StdEncoding.EncodeToString(key), true},
		{"raw url base64", base64.RawURLEncoding.EncodeToString(key), true},
		{"hex", hex.EncodeToString(key), true},
		{"too short", base64.StdEncoding.EncodeToString(key[:16]), false},
		{"garbage", "not a key!", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got, err := websearch.ParseKey(tc.in)

			// Assert
			if tc.ok != (err == nil) || (tc.ok && !bytes.Equal(got, key)) {
				t.Fatalf("got %x err %v", got, err)
			}
		})
	}
}

func TestDevSealerIsSharedBetweenProcesses(t *testing.T) {
	// Without a key (development) the api and the runtime derive the same key.
	// Arrange
	sealed, _ := websearch.DevSealer().Seal([]byte("t"), "r", "u")

	// Act
	plain, err := websearch.DevSealer().Open(sealed, "r", "u")

	// Assert
	if err != nil || string(plain) != "t" {
		t.Fatalf("%q %v", plain, err)
	}
}
