package aimod

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrNoSecretKey reports that MERLIN_SECRET_KEY is unset, so there is
// nowhere safe to put a guild's OpenRouter key.
//
// This fails closed on purpose. The alternative, storing the key in the
// clear because the operator did not set an env var, hands anyone with a
// database dump or a backup file a credential that spends real money and
// carries the message text of every server this bot moderates. An admin who
// hits this gets told what to set; nobody gets a silent downgrade.
var ErrNoSecretKey = errors.New("aimod: MERLIN_SECRET_KEY is not set, so an OpenRouter key cannot be stored")

// ErrSecretKeyChanged reports that a stored key could not be opened with the
// current MERLIN_SECRET_KEY. Almost always a rotated or mistyped env var
// rather than tampering, and the fix is the same either way: set the key
// again with /aimod configure key.
var ErrSecretKeyChanged = errors.New("aimod: stored API key cannot be decrypted with the current MERLIN_SECRET_KEY")

// secretKeyBytes is AES-256. Not a choice so much as the only size worth
// having here: the key is generated once by an operator running a one-line
// command, so a shorter one saves nothing.
const secretKeyBytes = 32

// sealer encrypts and decrypts guild API keys with AES-256-GCM.
//
// GCM rather than raw AES because the ciphertext sits in a database row that
// this process later trusts enough to send to a third party with money
// attached: it needs to detect a modified row, not just hide the contents.
// The nonce is prepended to the ciphertext, which is the standard layout and
// keeps the column a single opaque BYTEA.
type sealer struct {
	aead cipher.AEAD
}

// newSealer parses a base64 MERLIN_SECRET_KEY. A nil sealer is a valid
// state, returned for an empty key: the plugin runs its free local rungs
// and refuses only the commands that would need to store or read a secret.
func newSealer(base64Key string) (*sealer, error) {
	if base64Key == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("aimod: MERLIN_SECRET_KEY is not valid base64: %w", err)
	}
	if len(key) != secretKeyBytes {
		return nil, fmt.Errorf("aimod: MERLIN_SECRET_KEY decodes to %d bytes, want %d: generate one with `openssl rand -base64 32`", len(key), secretKeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aimod: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aimod: build GCM: %w", err)
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(plaintext string) ([]byte, error) {
	if s == nil {
		return nil, ErrNoSecretKey
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("aimod: read nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (s *sealer) open(sealed []byte) (string, error) {
	if s == nil {
		return "", ErrNoSecretKey
	}
	if len(sealed) < s.aead.NonceSize() {
		return "", ErrSecretKeyChanged
	}
	nonce, body := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	plaintext, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		// Deliberately not wrapped: GCM's own error says nothing useful and
		// the actionable cause is always the same one.
		return "", ErrSecretKeyChanged
	}
	return string(plaintext), nil
}

// maskKey renders a key for display. Never the whole thing, and never in a
// log line: this is what /aimod configure show and /aimod status print so an
// admin can tell which key is configured without the value being readable
// over a shoulder or in a screenshot.
func maskKey(key string) string {
	const tailLen = 4
	if len(key) <= tailLen {
		return "set"
	}
	return "..." + key[len(key)-tailLen:]
}
