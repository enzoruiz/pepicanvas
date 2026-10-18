// Package correlation carries one operation identifier through a context so
// logs from every layer of a request can be related (CTRL-TRACE-01 primitive).
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// ID is a canonical correlation identifier: 32 lowercase hex characters.
type ID string

// ErrInvalid reports an identifier that is not in canonical form.
var ErrInvalid = errors.New("correlation: invalid identifier")

// New returns a random identifier.
func New() (ID, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("correlation: read random bytes: %w", err)
	}
	return ID(hex.EncodeToString(buf)), nil
}

// Parse accepts only the canonical form, so an untrusted inbound value can
// never inject separators or secrets into log output.
func Parse(s string) (ID, error) {
	if len(s) != 32 {
		return "", ErrInvalid
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", ErrInvalid
		}
	}
	return ID(s), nil
}

// String returns the identifier text.
func (id ID) String() string { return string(id) }

type contextKey struct{}

// WithID returns a context carrying id.
func WithID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identifier carried by ctx, if any.
func FromContext(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(contextKey{}).(ID)
	return id, ok && id != ""
}
