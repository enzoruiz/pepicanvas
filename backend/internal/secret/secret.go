// Package secret wraps credential-bearing values so that no formatting,
// serialization, or logging path can render their raw content (TE-003).
package secret

import (
	"fmt"
	"io"
	"log/slog"
)

// Kind identifies a family of secret values.
type Kind string

const (
	KindInvitationToken Kind = "invitation_token"
	KindSessionToken    Kind = "session_token"
	KindMediaURL        Kind = "media_url"
	KindOBSCredential   Kind = "obs_credential"
)

// Valid reports whether the kind belongs to the closed vocabulary.
func (k Kind) Valid() bool {
	switch k {
	case KindInvitationToken, KindSessionToken, KindMediaURL, KindOBSCredential:
		return true
	default:
		return false
	}
}

// payload sits behind a pointer so reflection-based printing of structs that
// hold a Value in an unexported field shows an address, never the raw text.
type payload struct {
	kind Kind
	raw  string
}

// Value holds a secret. Its zero value is empty.
type Value struct {
	p *payload
}

// New wraps raw as a secret of the given kind. It panics on an unknown kind
// because that is a programming error, not an input error.
func New(kind Kind, raw string) Value {
	if !kind.Valid() {
		panic(fmt.Sprintf("secret: unknown kind %q", string(kind)))
	}
	return Value{p: &payload{kind: kind, raw: raw}}
}

// Reveal returns the raw secret. Call it only at the boundary that needs the
// raw bytes (hashing, cookie writing, outbound credential use).
func (v Value) Reveal() string {
	if v.p == nil {
		return ""
	}
	return v.p.raw
}

// Kind returns the secret's kind, or an empty kind for the zero value.
func (v Value) Kind() Kind {
	if v.p == nil {
		return ""
	}
	return v.p.kind
}

// IsZero reports whether the value holds no secret.
func (v Value) IsZero() bool {
	return v.p == nil
}

// String returns a redaction marker naming the kind.
func (v Value) String() string {
	if v.p == nil {
		return "[REDACTED]"
	}
	return "[REDACTED:" + string(v.p.kind) + "]"
}

// GoString keeps %#v redacted.
func (v Value) GoString() string { return v.String() }

// Format renders the redaction marker for every verb and flag.
func (v Value) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, v.String())
}

// MarshalText keeps text and JSON encoders redacted.
func (v Value) MarshalText() ([]byte, error) {
	return []byte(v.String()), nil
}

// LogValue keeps slog output redacted regardless of handler.
func (v Value) LogValue() slog.Value {
	return slog.StringValue(v.String())
}
