package secrets

import (
	"bytes"
	"strings"
	"testing"
)

// testKey returns a deterministic 32-byte key that is not all zeroes.
func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i*7 + 1)
	}
	return key
}

func TestCipherRoundtrip(t *testing.T) {
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	cases := map[string][]byte{
		"empty":       {},
		"short":       []byte("x"),
		"api key":     []byte("0123456789abcdef0123456789abcdef"),
		"utf8":        []byte("poprawny klucz — 日本語"),
		"binary body": bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 512),
	}
	for name, plaintext := range cases {
		t.Run(name, func(t *testing.T) {
			blob, err := c.Encrypt(plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if want := NonceSize + len(plaintext) + TagSize; len(blob) != want {
				t.Fatalf("blob length = %d, want %d (12B nonce || ct || tag)", len(blob), want)
			}
			got, err := c.Decrypt(blob)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("roundtrip mismatch: got %q want %q", got, plaintext)
			}
		})
	}
}

func TestCipherEncryptString(t *testing.T) {
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.EncryptString("s3cr3t")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != "s3cr3t" {
		t.Fatalf("got %q", got)
	}
}

func TestCipherNonceUniqueness(t *testing.T) {
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plaintext := []byte("same plaintext every time")
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		blob, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt #%d: %v", i, err)
		}
		nonce := string(blob[:NonceSize])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("duplicate nonce on iteration %d", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestCipherTamperDetect(t *testing.T) {
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plaintext := []byte("tamper-evident payload")
	blob, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tamper := map[string]func(b []byte) []byte{
		"nonce":            func(b []byte) []byte { b[0] ^= 0x01; return b },
		"first ct byte":    func(b []byte) []byte { b[NonceSize] ^= 0x80; return b },
		"last tag byte":    func(b []byte) []byte { b[len(b)-1] ^= 0x01; return b },
		"truncated":        func(b []byte) []byte { return b[:len(b)-1] },
		"shorter than hdr": func(b []byte) []byte { return b[:NonceSize-1] },
		"empty":            func(b []byte) []byte { return nil },
	}
	for name, mutate := range tamper {
		t.Run(name, func(t *testing.T) {
			// Copy so a mutation cannot leak into another subtest.
			bad := append([]byte(nil), blob...)
			bad = mutate(bad)
			if _, err := c.Decrypt(bad); err == nil {
				t.Fatal("Decrypt accepted tampered ciphertext")
			}
		})
	}
}

func TestCipherWrongKey(t *testing.T) {
	key := testKey(t)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c.Encrypt([]byte("bound to one key"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	other := append([]byte(nil), key...)
	other[0] ^= 0xff
	wrong, err := NewCipher(other)
	if err != nil {
		t.Fatalf("NewCipher(other): %v", err)
	}
	if _, err := wrong.Decrypt(blob); err == nil {
		t.Fatal("Decrypt succeeded with the wrong key")
	}
}

func TestNewCipherRejectsInvalidKeyLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 24, 31, 33, 64} {
		if _, err := NewCipher(make([]byte, n)); err == nil {
			t.Fatalf("NewCipher accepted a %d-byte key, want error", n)
		}
	}
}

func TestMaskNeverRevealsSecret(t *testing.T) {
	cases := []string{"", "a", "abcd", "abcde", "0123456789abcdef", strings.Repeat("k", 64)}
	for _, secret := range cases {
		masked := Mask(secret)
		if masked == secret && secret != "" {
			t.Fatalf("Mask(%q) returned the secret unchanged", secret)
		}
		if secret != "" && strings.Contains(masked, secret) {
			t.Fatalf("Mask(%q) = %q leaks the whole secret", secret, masked)
		}
		if len(secret) > 4 {
			if want := secret[len(secret)-4:]; !strings.HasSuffix(masked, want) {
				t.Fatalf("Mask(%q) = %q, want suffix %q", secret, masked, want)
			}
		} else if strings.ContainsAny(masked, secret) {
			t.Fatalf("Mask(%q) = %q leaks characters of a short secret", secret, masked)
		}
	}
	if Mask("abcdefgh") == Mask("abcdefgi") {
		t.Fatal("Mask ignores the last four characters")
	}
}

func TestLast4(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"ab":               "",
		"abcd":             "",
		"0123456789abcdef": "cdef",
	}
	for in, want := range cases {
		if got := Last4(in); got != want {
			t.Fatalf("Last4(%q) = %q, want %q", in, got, want)
		}
	}
}
