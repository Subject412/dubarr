package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
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

// Maximum cost parameters accepted from a stored PHC string. They sit well
// above the defaults so hashes can be re-costed later, but they stop a corrupt
// or hostile hash from making VerifyPassword allocate gigabytes or iterate for
// hours: a stored hash must never be able to take the process down.
const (
	maxArgonMemory  uint32 = 256 * 1024 // KiB
	maxArgonTime    uint32 = 10
	maxArgonThreads uint8  = 16
	maxArgonSaltLen        = 64
	maxArgonKeyLen         = 1024
)

// argonParams is the parsed form of a verified-format Argon2id PHC string.
type argonParams struct {
	memory     uint32
	iterations uint32
	threads    uint8
	salt       []byte
	key        []byte
}

// hashPassword is the hashing seam the setup handler calls; tests replace it
// to prove an already-initialised appliance rejects setup without paying the
// Argon2id cost.
var hashPassword = HashPassword

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
// raised later (up to maxArgon*).
func VerifyPassword(encoded, password string) (bool, error) {
	p, err := parseArgon2id(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), p.salt, p.iterations, p.memory, p.threads, uint32(len(p.key)))
	if subtle.ConstantTimeCompare(got, p.key) != 1 {
		return false, nil
	}
	return true, nil
}

// parseArgon2id strictly parses a stored PHC string, accepting only the exact
// canonical layout:
//
//	$argon2id$v=19$m=<n>,t=<n>,p=<n>$<b64 salt>$<b64 hash>
//
// fmt.Sscanf-style parsing accepts trailing garbage and reordered fields, and
// an untrusted m=/t= can make argon2.IDKey allocate or spin arbitrarily long,
// so every field, its order and its bound are checked explicitly here.
func parseArgon2id(encoded string) (argonParams, error) {
	var p argonParams

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, ErrInvalidHash
	}

	version, ok := strings.CutPrefix(parts[2], "v=")
	if !ok || version != strconv.FormatUint(uint64(argon2.Version), 10) {
		return p, ErrInvalidHash
	}

	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return p, ErrInvalidHash
	}
	names := [3]string{"m=", "t=", "p="}
	values := [3]uint64{}
	for i, name := range names {
		value, ok := strings.CutPrefix(fields[i], name)
		if !ok || value == "" {
			return p, ErrInvalidHash
		}
		n, err := strconv.ParseUint(value, 10, 32)
		// Reject non-canonical spellings (leading zeros, a "+" sign, ...) so
		// the stored representation has exactly one accepted form.
		if err != nil || strconv.FormatUint(n, 10) != value {
			return p, ErrInvalidHash
		}
		values[i] = n
	}
	// Zero is meaningless for every parameter; absurd values are a DoS vector.
	if values[0] == 0 || values[1] == 0 || values[2] == 0 ||
		values[0] > uint64(maxArgonMemory) ||
		values[1] > uint64(maxArgonTime) ||
		values[2] > uint64(maxArgonThreads) {
		return p, ErrInvalidHash
	}

	salt, err := decodeStdB64(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > maxArgonSaltLen {
		return p, ErrInvalidHash
	}
	key, err := decodeStdB64(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxArgonKeyLen {
		return p, ErrInvalidHash
	}

	p.memory = uint32(values[0])
	p.iterations = uint32(values[1])
	p.threads = uint8(values[2])
	p.salt = salt
	p.key = key
	return p, nil
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
