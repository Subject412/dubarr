package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (PLAN.md T3): 64 MiB, one pass, four lanes, 32-byte key,
// 16-byte CSPRNG salt.
const (
	argonMemory  uint32 = 64 * 1024
	argonTime    uint32 = 1
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrInvalidHash reports a stored hash that is not a well-formed Argon2id PHC
// string. Callers treat it as an authentication failure, never as a panic.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// HashPassword derives an Argon2id hash of password and returns it as a
// standard PHC string:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<b64 salt>$<b64 hash>
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the Argon2id PHC string in
// encoded. It re-derives the key with the parameters recorded in the hash and
// compares in constant time, so it stays correct if the cost parameters are
// raised later.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidHash
	}

	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, ErrInvalidHash
	}
	if memory == 0 || iterations == 0 || threads == 0 {
		return false, ErrInvalidHash
	}

	salt, err := decodeStdB64(parts[4])
	if err != nil || len(salt) == 0 {
		return false, ErrInvalidHash
	}
	want, err := decodeStdB64(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrInvalidHash
	}

	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	return true, nil
}

// decodeStdB64 accepts both padded and unpadded standard base64.
func decodeStdB64(s string) ([]byte, error) {
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// dummyPasswordHash returns a throwaway Argon2id hash used to equalise the cost
// of a login attempt for an unknown username, so response timing does not
// reveal whether an account exists.
func dummyPasswordHash() string {
	dummyHashOnce.Do(func() {
		h, err := HashPassword("dubarr-login-timing-equaliser")
		if err != nil {
			dummyHash = ""
			return
		}
		dummyHash = h
	})
	return dummyHash
}
