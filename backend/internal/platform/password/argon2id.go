// Package password hashes account passwords with argon2id (US-001).
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// OWASP minimum argon2id configuration: 19 MiB, two passes, one lane.
const (
	DefaultMemory      = 19 * 1024
	DefaultIterations  = 2
	DefaultParallelism = 1

	saltBytes = 16
	keyBytes  = 32

	// Bounds accepted when verifying, so a stored hash cannot demand
	// unbounded memory or time.
	maxMemory      = 256 * 1024
	maxIterations  = 10
	maxParallelism = 16
)

// ErrMalformedHash reports a stored hash that is not a supported argon2id PHC string.
var ErrMalformedHash = errors.New("password: malformed hash")

// Argon2id implements access.PasswordHasher. Zero fields use the defaults.
// Verification reads the parameters stored in each hash.
type Argon2id struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
}

func (a Argon2id) params() (uint32, uint32, uint8) {
	m, t, p := a.Memory, a.Iterations, a.Parallelism
	if m == 0 {
		m = DefaultMemory
	}
	if t == 0 {
		t = DefaultIterations
	}
	if p == 0 {
		p = DefaultParallelism
	}
	return m, t, p
}

// Hash returns the PHC string $argon2id$v=19$m=..,t=..,p=..$salt$key.
func (a Argon2id) Hash(password secret.Value) (string, error) {
	m, t, p := a.params()
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password.Reveal()), salt, t, m, p, keyBytes)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, m, t, p, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify reports whether password matches hash in constant time.
func (Argon2id) Verify(hash string, password secret.Value) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformedHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrMalformedHash
	}
	if m < 8*uint32(p) || m > maxMemory || t < 1 || t > maxIterations || p < 1 || p > maxParallelism {
		return false, ErrMalformedHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, ErrMalformedHash
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(password.Reveal()), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
