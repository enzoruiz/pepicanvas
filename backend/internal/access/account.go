package access

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// Errors returned by the account and session service (US-001).
var (
	// ErrInvalidRegistration reports a registration rejected before storage.
	ErrInvalidRegistration = errors.New("access: invalid registration")
	// ErrIdentityTaken reports an email that already belongs to an account.
	ErrIdentityTaken = errors.New("access: identity already registered")
	// ErrOperationConflict reports an operation ID reused for a different
	// registration. A new intent requires a new operation ID.
	ErrOperationConflict = errors.New("access: operation id reused with a different registration")
	// ErrInvalidCredentials is the only sign-in failure. It never says
	// whether the email exists.
	ErrInvalidCredentials = errors.New("access: invalid credentials")
	// ErrSessionInvalid reports a missing, malformed, signed-out, or expired session.
	ErrSessionInvalid = errors.New("access: session invalid")
	// ErrOperationRetained is returned by AccountStore.CreateAccount when the
	// operation ID is still retained by another registration.
	ErrOperationRetained = errors.New("access: operation id retained")
)

// Bounds and lifetimes of accounts and sessions.
const (
	MaxEmailBytes           = 254
	MinPasswordChars        = 12
	MaxPasswordChars        = 128
	MinOperationIDBytes     = 16
	MaxOperationIDBytes     = 128
	SessionIdleTimeout      = 24 * time.Hour
	SessionAbsoluteLifetime = 7 * 24 * time.Hour
	// RegistrationRetention is how long a registration operation ID replays
	// its account. An operation accepted at T is retained while now < T + ttl.
	RegistrationRetention = 10 * time.Minute
)

// AccountID identifies one account.
type AccountID string

// Account is a registered identity. PasswordHash is an argon2id PHC string.
type Account struct {
	ID           AccountID
	Email        string
	PasswordHash string
}

// Registration is the retained result of one registration operation. Only
// the SHA-256 of the normalized email is kept; a replay is equivalent when
// the email digest matches and the password verifies against the account.
type Registration struct {
	OperationID string
	EmailDigest [32]byte
	AccountID   AccountID
	AcceptedAt  time.Time
}

// NewAccount is one account and its registration, committed together.
type NewAccount struct {
	Account      Account
	Registration Registration
	// CorrelationID is the request correlation (CTRL-TRACE-01 primitive), or empty.
	CorrelationID string
	// PurgeBefore drops registrations accepted at or before it.
	PurgeBefore time.Time
}

// SessionRecord is the stored form of a session: only the token's SHA-256.
type SessionRecord struct {
	TokenHash  [32]byte
	AccountID  AccountID
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// AccountStore persists accounts and registration operations.
type AccountStore interface {
	// FindRegistration returns the registration of operationID accepted after notBefore.
	FindRegistration(ctx context.Context, operationID string, notBefore time.Time) (Registration, bool, error)
	// CreateAccount commits the account and its registration in one
	// transaction. It returns ErrIdentityTaken for a registered email and
	// ErrOperationRetained for a retained operation ID.
	CreateAccount(ctx context.Context, account NewAccount) error
	AccountByEmail(ctx context.Context, email string) (Account, bool, error)
	AccountByID(ctx context.Context, id AccountID) (Account, bool, error)
}

// SessionStore persists sessions by token hash.
type SessionStore interface {
	CreateSession(ctx context.Context, session SessionRecord) error
	SessionByHash(ctx context.Context, hash [32]byte) (SessionRecord, bool, error)
	// TouchSession moves LastSeenAt forward to at; it never moves it back.
	TouchSession(ctx context.Context, hash [32]byte, at time.Time) error
	DeleteSession(ctx context.Context, hash [32]byte) error
}

// PasswordHasher derives and verifies password hashes.
type PasswordHasher interface {
	Hash(password secret.Value) (string, error)
	Verify(hash string, password secret.Value) (bool, error)
}

// NormalizeEmail trims and lower-cases an email and checks its shape. The
// check is deliberately structural: delivery is not verified (US-001 non-goal).
func NormalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > MaxEmailBytes || !utf8.ValidString(email) {
		return "", false
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", false
	}
	return email, true
}

func validPassword(password secret.Value) bool {
	if password.Kind() != secret.KindPassword {
		return false
	}
	raw := password.Reveal()
	if !utf8.ValidString(raw) {
		return false
	}
	n := utf8.RuneCountInString(raw)
	return n >= MinPasswordChars && n <= MaxPasswordChars
}

// ValidOperationID reports whether id is 16-128 bytes of [A-Za-z0-9_-].
func ValidOperationID(id string) bool {
	if len(id) < MinOperationIDBytes || len(id) > MaxOperationIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
