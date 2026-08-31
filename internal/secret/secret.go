// Package secret seals small, high-value strings for storage in Postgres.
//
// It exists because more than one plugin now has something worth encrypting
// at rest and none of them should be carrying their own crypto: aimod stores
// a gateway API key that spends real money, and contest stores a prize code
// (a Steam key, a Nitro gift link) that is worth exactly as much as whoever
// reads it first. Both are opaque BYTEA columns this process later trusts
// enough to hand to a third party or to a member, so both need the same two
// properties, and neither is worth a second implementation.
//
// A nil *Sealer is a valid state, returned when MERLIN_SECRET_KEY is unset.
// It fails every Seal and Open with ErrNoKey rather than panicking, so a
// plugin can keep running the parts of itself that need no secret and refuse
// only the commands that do.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrNoKey reports that MERLIN_SECRET_KEY is unset, so there is nowhere safe
// to put the value.
//
// This fails closed on purpose. The alternative, storing the value in the
// clear because the operator did not set an env var, hands anyone with a
// database dump or a backup file a credential that is live. An admin who
// hits this gets told what to set; nobody gets a silent downgrade.
var ErrNoKey = errors.New("secret: MERLIN_SECRET_KEY is not set, so nothing can be stored encrypted")

// ErrKeyChanged reports that a stored value could not be opened with the
// current MERLIN_SECRET_KEY. Almost always a rotated or mistyped env var
// rather than tampering, and the fix is the same either way: set the value
// again through whichever command stored it.
var ErrKeyChanged = errors.New("secret: the stored value cannot be decrypted with the current MERLIN_SECRET_KEY")

// KeyBytes is AES-256. Not a choice so much as the only size worth having
// here: the key is generated once by an operator running a one-line command,
// so a shorter one saves nothing.
const KeyBytes = 32

// Sealer encrypts and decrypts stored values with AES-256-GCM.
//
// GCM rather than raw AES because the ciphertext sits in a database row that
// this process later trusts: it needs to detect a modified row, not just
// hide the contents. The nonce is prepended to the ciphertext, which is the
// standard layout and keeps the column a single opaque BYTEA.
type Sealer struct {
	aead cipher.AEAD
}

// New parses a base64 MERLIN_SECRET_KEY. A nil Sealer is a valid state,
// returned for an empty key.
func New(base64Key string) (*Sealer, error) {
	if base64Key == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("secret: MERLIN_SECRET_KEY is not valid base64: %w", err)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("secret: MERLIN_SECRET_KEY decodes to %d bytes, want %d: generate one with `openssl rand -base64 32`", len(key), KeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: build GCM: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext. The result is safe to store as one BYTEA column.
func (s *Sealer) Seal(plaintext string) ([]byte, error) {
	if s == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: read nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a value produced by Seal.
func (s *Sealer) Open(sealed []byte) (string, error) {
	if s == nil {
		return "", ErrNoKey
	}
	if len(sealed) < s.aead.NonceSize() {
		return "", ErrKeyChanged
	}
	nonce, body := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	plaintext, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		// Deliberately not wrapped: GCM's own error says nothing useful and
		// the actionable cause is always the same one.
		return "", ErrKeyChanged
	}
	return string(plaintext), nil
}

// Mask renders a stored value for display. Never the whole thing, and never
// in a log line: this is what a `show` or `status` command prints so an
// admin can tell which value is configured without it being readable over a
// shoulder or in a screenshot.
func Mask(value string) string {
	const tailLen = 4
	if len(value) <= tailLen {
		return "set"
	}
	return "..." + value[len(value)-tailLen:]
}
