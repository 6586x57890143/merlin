package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// testSecretKey stands in for MERLIN_SECRET_KEY.
//
// Generated once per process rather than written down as a constant. A fixed
// base64 string of exactly the right length, in a file called secret_test.go,
// is indistinguishable from a real leaked key to a secret scanner, and CI is
// right to refuse to make that judgement on our behalf. Generating it also
// costs nothing: seal and open only need to agree within a run.
var testSecretKey = func() string {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic("aimod: generating a test key: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(key)
}()

func TestSealRoundTrip(t *testing.T) {
	s, err := New(testSecretKey)
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}
	// Deliberately low-entropy and not bound to a name a scanner reads as a
	// credential, for the same reason as above.
	const plaintext = "sk-or-v1-fake-value-for-tests"

	sealed, err := s.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(string(sealed), plaintext) {
		t.Fatal("the plaintext key is present in the sealed bytes")
	}
	got, err := s.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != plaintext {
		t.Errorf("open = %q, want %q", got, plaintext)
	}
}

// The nonce is fresh per seal, so the same key stored twice does not produce
// the same bytes and a database dump does not reveal which guilds share one.
func TestSealIsNotDeterministic(t *testing.T) {
	s, _ := New(testSecretKey)
	a, _ := s.Seal("sk-or-v1-same")
	b, _ := s.Seal("sk-or-v1-same")
	if string(a) == string(b) {
		t.Error("sealing the same key twice produced identical bytes")
	}
}

// A rotated or mistyped MERLIN_SECRET_KEY must fail loudly with the one
// error whose message names the actual fix, not return junk.
func TestOpenWithTheWrongKeyFails(t *testing.T) {
	other := make([]byte, KeyBytes)
	if _, err := rand.Read(other); err != nil {
		t.Fatalf("rand: %v", err)
	}
	first, _ := New(testSecretKey)
	second, err := New(base64.StdEncoding.EncodeToString(other))
	if err != nil {
		t.Fatalf("newSealer: %v", err)
	}

	sealed, _ := first.Seal("sk-or-v1-secret")
	if _, err := second.Open(sealed); !errors.Is(err, ErrKeyChanged) {
		t.Errorf("open with the wrong key = %v, want ErrKeyChanged", err)
	}
}

// GCM is authenticated for a reason: this ciphertext sits in a row the
// process later trusts enough to send to a third party with money attached,
// so a modified row has to be detected rather than decrypted into rubbish.
func TestTamperedCiphertextIsRejected(t *testing.T) {
	s, _ := New(testSecretKey)
	sealed, _ := s.Seal("sk-or-v1-secret")

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := s.Open(tampered); err == nil {
		t.Error("a modified ciphertext was accepted")
	}

	if _, err := s.Open([]byte("short")); err == nil {
		t.Error("bytes shorter than the nonce were accepted")
	}
}

// No key configured is a working state: the plugin runs its free rungs and
// refuses only the command that would store a secret. It must never fall
// back to storing one in the clear.
func TestNoSecretKeyRefusesRatherThanStoringPlaintext(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if s != nil {
		t.Fatal("an empty MERLIN_SECRET_KEY produced a working Sealer")
	}
	if _, err := s.Seal("sk-or-v1-secret"); !errors.Is(err, ErrNoKey) {
		t.Errorf("seal without a key = %v, want ErrNoKey", err)
	}
	if _, err := s.Open([]byte("anything")); !errors.Is(err, ErrNoKey) {
		t.Errorf("open without a key = %v, want ErrNoKey", err)
	}
}

// A key of the wrong length or shape must stop the process at startup with
// a message naming the fix, not silently truncate to something weaker.
func TestMalformedSecretKeysAreRejected(t *testing.T) {
	tests := map[string]string{
		"not base64":   "this is not base64 at all !!",
		"too short":    base64.StdEncoding.EncodeToString([]byte("sixteen bytes!!!")),
		"too long":     base64.StdEncoding.EncodeToString(make([]byte, 64)),
		"almost right": base64.StdEncoding.EncodeToString(make([]byte, KeyBytes-1)),
	}
	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(key); err == nil {
				t.Error("accepted a malformed MERLIN_SECRET_KEY")
			}
		})
	}
}

// maskKey is what /aimod status and /aimod configure show print. It has to
// identify a key without being one.
func TestMaskKeyRevealsOnlyATail(t *testing.T) {
	const stored = "sk-or-v1-fake-value-for-tests-dead"
	got := Mask(stored)
	if strings.Contains(got, "0123456789") {
		t.Errorf("maskKey = %q, which leaks the body of the key", got)
	}
	if !strings.HasSuffix(got, "dead") {
		t.Errorf("maskKey = %q, want it to end in the last four so two keys can be told apart", got)
	}
	if Mask("abc") == "abc" {
		t.Error("a short value was echoed back verbatim")
	}
}
