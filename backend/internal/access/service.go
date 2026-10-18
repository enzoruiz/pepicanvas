package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// csrfLabel separates the CSRF derivation from any other use of the token.
const csrfLabel = "pepicanvas/csrf/v1\x00"

var sessionTokenPattern = regexp.MustCompile(`^pcs_[A-Za-z0-9_-]{43}$`)

// RegisterRequest is one registration attempt.
type RegisterRequest struct {
	OperationID string
	Email       string
	Password    secret.Value
}

// Session is a newly created session. Token and CSRF never leave the server
// except in the session cookie and the response that created it.
type Session struct {
	AccountID AccountID
	Token     secret.Value
	CSRF      secret.Value
}

// Principal is the account behind a valid session.
type Principal struct {
	AccountID AccountID
	Email     string
}

// Service registers accounts and manages sessions.
type Service struct {
	accounts  AccountStore
	sessions  SessionStore
	hasher    PasswordHasher
	now       func() time.Time
	dummyHash string
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithClock sets the time source for expiry and retention.
func WithClock(now func() time.Time) ServiceOption {
	return func(s *Service) { s.now = now }
}

// NewService builds the service. It derives a dummy hash so an unknown email
// costs the same password verification as a known one.
func NewService(accounts AccountStore, sessions SessionStore, hasher PasswordHasher, options ...ServiceOption) (*Service, error) {
	s := &Service{accounts: accounts, sessions: sessions, hasher: hasher, now: time.Now}
	for _, option := range options {
		option(s)
	}
	filler, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	s.dummyHash, err = hasher.Hash(secret.New(secret.KindPassword, filler))
	if err != nil {
		return nil, fmt.Errorf("access: derive dummy hash: %w", err)
	}
	return s, nil
}

// Register creates an account, or replays the account of a retained,
// equivalent registration under the same operation ID.
func (s *Service) Register(ctx context.Context, request RegisterRequest) (AccountID, error) {
	email, ok := NormalizeEmail(request.Email)
	if !ok || !validPassword(request.Password) || !ValidOperationID(request.OperationID) {
		return "", ErrInvalidRegistration
	}
	digest := sha256.Sum256([]byte(email))

	now := s.now()
	if id, found, err := s.replay(ctx, request, digest, now); found || err != nil {
		return id, err
	}

	hash, err := s.hasher.Hash(request.Password)
	if err != nil {
		return "", fmt.Errorf("access: hash password: %w", err)
	}
	raw, err := randomHex(16)
	if err != nil {
		return "", err
	}
	id := AccountID(raw)
	correlationID, _ := correlation.FromContext(ctx)
	err = s.accounts.CreateAccount(ctx, NewAccount{
		Account:       Account{ID: id, Email: email, PasswordHash: hash},
		Registration:  Registration{OperationID: request.OperationID, EmailDigest: digest, AccountID: id, AcceptedAt: now},
		CorrelationID: correlationID.String(),
		PurgeBefore:   now.Add(-RegistrationRetention),
	})
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, ErrIdentityTaken), errors.Is(err, ErrOperationRetained):
		// A concurrent duplicate may have committed the same operation.
		if replayed, found, rerr := s.replay(ctx, request, digest, now); found || rerr != nil {
			return replayed, rerr
		}
		if errors.Is(err, ErrOperationRetained) {
			return "", ErrOperationConflict
		}
		return "", ErrIdentityTaken
	default:
		return "", fmt.Errorf("access: create account: %w", err)
	}
}

func (s *Service) replay(ctx context.Context, request RegisterRequest, digest [32]byte, now time.Time) (AccountID, bool, error) {
	registration, found, err := s.accounts.FindRegistration(ctx, request.OperationID, now.Add(-RegistrationRetention))
	if err != nil {
		return "", false, fmt.Errorf("access: find registration: %w", err)
	}
	if !found {
		return "", false, nil
	}
	if subtle.ConstantTimeCompare(registration.EmailDigest[:], digest[:]) != 1 {
		return "", true, ErrOperationConflict
	}
	account, ok, err := s.accounts.AccountByID(ctx, registration.AccountID)
	if err != nil {
		return "", true, fmt.Errorf("access: load account: %w", err)
	}
	if !ok {
		return "", true, ErrOperationConflict
	}
	match, err := s.hasher.Verify(account.PasswordHash, request.Password)
	if err != nil {
		return "", true, fmt.Errorf("access: verify password: %w", err)
	}
	if !match {
		return "", true, ErrOperationConflict
	}
	return registration.AccountID, true, nil
}

// SignIn verifies credentials and creates a session. Every failure returns
// ErrInvalidCredentials after exactly one password verification.
func (s *Service) SignIn(ctx context.Context, rawEmail string, password secret.Value) (Session, error) {
	email, emailOK := NormalizeEmail(rawEmail)
	passwordOK := validPassword(password)
	hash := s.dummyHash
	var account Account
	if emailOK && passwordOK {
		found, ok, err := s.accounts.AccountByEmail(ctx, email)
		if err != nil {
			return Session{}, fmt.Errorf("access: load account: %w", err)
		}
		if ok {
			account, hash = found, found.PasswordHash
		}
	}
	match, err := s.hasher.Verify(hash, secret.New(secret.KindPassword, password.Reveal()))
	if err != nil {
		return Session{}, fmt.Errorf("access: verify password: %w", err)
	}
	if !match || account.ID == "" || !passwordOK {
		return Session{}, ErrInvalidCredentials
	}

	token, err := secret.Generate(secret.KindSessionToken)
	if err != nil {
		return Session{}, err
	}
	now := s.now()
	if err := s.sessions.CreateSession(ctx, SessionRecord{
		TokenHash:  hashToken(token),
		AccountID:  account.ID,
		CreatedAt:  now,
		LastSeenAt: now,
	}); err != nil {
		return Session{}, fmt.Errorf("access: create session: %w", err)
	}
	return Session{AccountID: account.ID, Token: token, CSRF: CSRFToken(token)}, nil
}

// Authenticate returns the principal of a valid session and renews its idle
// window. An expired session is deleted.
func (s *Service) Authenticate(ctx context.Context, token secret.Value) (Principal, error) {
	if !validSessionToken(token) {
		return Principal{}, ErrSessionInvalid
	}
	hash := hashToken(token)
	record, ok, err := s.sessions.SessionByHash(ctx, hash)
	if err != nil {
		return Principal{}, fmt.Errorf("access: load session: %w", err)
	}
	if !ok {
		return Principal{}, ErrSessionInvalid
	}
	now := s.now()
	if !now.Before(record.LastSeenAt.Add(SessionIdleTimeout)) || !now.Before(record.CreatedAt.Add(SessionAbsoluteLifetime)) {
		if err := s.sessions.DeleteSession(ctx, hash); err != nil {
			return Principal{}, fmt.Errorf("access: delete expired session: %w", err)
		}
		return Principal{}, ErrSessionInvalid
	}
	account, ok, err := s.accounts.AccountByID(ctx, record.AccountID)
	if err != nil {
		return Principal{}, fmt.Errorf("access: load account: %w", err)
	}
	if !ok {
		return Principal{}, ErrSessionInvalid
	}
	if err := s.sessions.TouchSession(ctx, hash, now); err != nil {
		return Principal{}, fmt.Errorf("access: renew session: %w", err)
	}
	return Principal{AccountID: account.ID, Email: account.Email}, nil
}

// SignOut deletes the session. Signing out an unknown session succeeds.
func (s *Service) SignOut(ctx context.Context, token secret.Value) error {
	if !validSessionToken(token) {
		return nil
	}
	if err := s.sessions.DeleteSession(ctx, hashToken(token)); err != nil {
		return fmt.Errorf("access: delete session: %w", err)
	}
	return nil
}

// CSRFToken derives the CSRF token bound to a session. It needs no storage:
// forging it requires the session token, which only the HttpOnly cookie holds.
func CSRFToken(token secret.Value) secret.Value {
	if !validSessionToken(token) {
		return secret.Value{}
	}
	sum := sha256.Sum256([]byte(csrfLabel + token.Reveal()))
	return secret.New(secret.KindCSRFToken, base64.RawURLEncoding.EncodeToString(sum[:]))
}

// VerifyCSRF reports whether presented is the CSRF token of the session.
func VerifyCSRF(token secret.Value, presented string) bool {
	want := CSRFToken(token)
	if want.IsZero() || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want.Reveal()), []byte(presented)) == 1
}

func validSessionToken(token secret.Value) bool {
	return token.Kind() == secret.KindSessionToken && sessionTokenPattern.MatchString(token.Reveal())
}

func hashToken(token secret.Value) [32]byte {
	return sha256.Sum256([]byte(token.Reveal()))
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("access: read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
