// Package locksession keeps the lock sessions of browsers that edit over
// HTTP. A lock session stands in for a connection until a WebSocket exists
// (US-017): it lives while its browser keeps sending requests, is bound to
// a TE-002 registry lease so an access cut ends it, and owns one
// locking.Session, so every lock it holds is freed the moment it ends. A
// reconnection opens a new session and never gets the old locks back.
package locksession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// Defaults applied when a Config field is zero.
const (
	DefaultIdle          = locking.DefaultTTL
	DefaultMaxPerAccount = 4
	DefaultMaxSessions   = 10000
)

// idPrefix marks lock session IDs, which are opaque and never tokens.
const idPrefix = "pls_"

var (
	// ErrInvalidConfig reports a directory configuration that cannot work.
	ErrInvalidConfig = errors.New("locksession: invalid config")
	// ErrGone reports a lock session that ended or never belonged to the
	// caller. Its locks are already free; the client must open a new one.
	ErrGone = errors.New("locksession: session gone")
	// ErrFull reports that the directory holds as many sessions as allowed.
	ErrFull = errors.New("locksession: too many sessions")
	// ErrClosed reports work submitted after the directory began shutting
	// down.
	ErrClosed = errors.New("locksession: directory closed")
)

// Registrar admits connections to the TE-002 registry.
type Registrar interface {
	Register(grant revocation.Grant, conn revocation.Conn) (*revocation.Lease, error)
}

// Config configures a Directory.
type Config struct {
	// Manager holds the locks; it is required.
	Manager *locking.Manager
	// Registry applies access cuts to the sessions; it is required.
	Registry Registrar
	// Idle ends a session that saw no Open or Use for this long. It
	// defaults to DefaultIdle.
	Idle time.Duration
	// MaxPerAccount bounds the sessions of one account in one tenant;
	// opening one more ends the oldest. It defaults to DefaultMaxPerAccount.
	MaxPerAccount int
	// MaxSessions bounds every session of the process. It defaults to
	// DefaultMaxSessions.
	MaxSessions int
	// Clock defaults to the system clock.
	Clock locking.Clock
	// Logger records every session opened and ended. It should be built on
	// platform/logging so records carry the correlation_id and are redacted.
	// It defaults to discarding.
	Logger *slog.Logger
}

// reason explains why a session ended.
type reason string

const (
	reasonIdle     reason = "idle"
	reasonEnded    reason = "ended"
	reasonEvicted  reason = "evicted"
	reasonRevoked  reason = "revoked"
	reasonShutdown reason = "shutdown"
)

type systemClock struct{}

func (systemClock) Now() time.Time                                    { return time.Now() }
func (systemClock) AfterFunc(d time.Duration, f func()) locking.Timer { return time.AfterFunc(d, f) }

// Directory holds every lock session of the process. Close it before the
// locking manager and the registry, so its sessions end as shutdown rather
// than as detached or cut connections.
type Directory struct {
	config Config

	mu       sync.Mutex
	closed   bool
	sessions map[string]*entry
	// accounts lists each account's sessions, oldest first.
	accounts map[scope][]*entry
}

type scope struct {
	tenantID  string
	accountID string
}

type entry struct {
	id      string
	scope   scope
	name    string
	lease   *revocation.Lease
	session *locking.Session

	// Guarded by Directory.mu.
	deadline time.Time
	timer    locking.Timer
	// ctx carries the values of the last Open or Use, so the asynchronous
	// idle end logs its correlation.
	ctx context.Context
}

// New validates config and returns an empty directory.
func New(config Config) (*Directory, error) {
	if config.Manager == nil || config.Registry == nil {
		return nil, fmt.Errorf("%w: manager and registry are required", ErrInvalidConfig)
	}
	if config.Idle < 0 || config.MaxPerAccount < 0 || config.MaxSessions < 0 {
		return nil, fmt.Errorf("%w: negative idle or bound", ErrInvalidConfig)
	}
	if config.Idle == 0 {
		config.Idle = DefaultIdle
	}
	if config.MaxPerAccount == 0 {
		config.MaxPerAccount = DefaultMaxPerAccount
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = DefaultMaxSessions
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	return &Directory{config: config, sessions: map[string]*entry{}, accounts: map[scope][]*entry{}}, nil
}

// Open registers an editor grant with the registry and returns the ID of a
// new lock session bound to the registry lease. The session's locks are
// held as grant's account, with the ID as the holder session, and name is
// the account's display name shown to others. Registry errors are returned
// unchanged: a grant read before an applied cut is rejected. Opening one
// session more than MaxPerAccount ends that account's oldest session first.
func (d *Directory) Open(ctx context.Context, grant revocation.Grant, name string) (string, error) {
	if grant.Client != revocation.ClientEditor {
		return "", fmt.Errorf("%w: lock sessions need an editor grant", revocation.ErrInvalidGrant)
	}
	s := scope{tenantID: grant.TenantID, accountID: grant.AccountID}
	d.mu.Lock()
	err := d.admitLocked(s)
	d.mu.Unlock()
	if err != nil {
		return "", err
	}

	e := &entry{id: newID(), scope: s, name: name}
	lease, err := d.config.Registry.Register(grant, conn{directory: d, entry: e})
	if err != nil {
		return "", err
	}
	e.lease = lease
	session, err := d.config.Manager.Attach(lease.Context(), locking.Holder{TenantID: s.tenantID, AccountID: s.accountID, SessionID: e.id})
	if err != nil {
		lease.Release()
		return "", err
	}
	e.session = session

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.admitLocked(s); err != nil {
		d.discard(e)
		return "", err
	}
	if lease.Err() != nil {
		// A cut reached the lease before the entry was known, so its close
		// found nothing to end.
		d.discard(e)
		return "", lease.Err()
	}
	if accountSessions := d.accounts[s]; len(accountSessions) >= d.config.MaxPerAccount {
		d.endLocked(ctx, accountSessions[0], reasonEvicted)
	}
	d.sessions[e.id] = e
	d.accounts[s] = append(d.accounts[s], e)
	d.renewLocked(ctx, e, d.config.Clock.Now())
	d.config.Logger.InfoContext(ctx, "lock session opened", slog.String("tenant_id", s.tenantID))
	return e.id, nil
}

// Use runs fn with the live session behind id and counts as a sign of life,
// moving its idle deadline to Idle from now. An unknown or ended ID, or the
// ID of another tenant or account, returns ErrGone without calling fn. fn
// runs outside the directory lock and may see locking.ErrDetached if the
// session ends meanwhile; its error is returned unchanged.
func (d *Directory) Use(ctx context.Context, id, tenantID, accountID string, fn func(*locking.Session) error) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	e := d.liveLocked(id, d.config.Clock.Now())
	if e == nil || e.scope != (scope{tenantID: tenantID, accountID: accountID}) {
		d.mu.Unlock()
		return ErrGone
	}
	d.renewLocked(ctx, e, d.config.Clock.Now())
	session := e.session
	d.mu.Unlock()
	return fn(session)
}

// End disconnects the session behind id at once, freeing its locks: the
// client closed it, or the transport saw its actor lose access. It is
// idempotent and ignores IDs of another tenant or account.
func (d *Directory) End(ctx context.Context, id, tenantID, accountID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.sessions[id]; e != nil && e.scope == (scope{tenantID: tenantID, accountID: accountID}) {
		d.endLocked(ctx, e, reasonEnded)
	}
}

// Name returns the display name of the live session behind a lock holder's
// session ID (locking.Holder.SessionID). Callers show it only to members of
// the holder's tenant.
func (d *Directory) Name(sessionID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.liveLocked(sessionID, d.config.Clock.Now()); e != nil {
		return e.name, true
	}
	return "", false
}

// Close rejects Open and Use with ErrClosed at once, ends every session,
// releasing its registry lease and its locks, and stops every idle timer.
// It never blocks, so it returns nil; it is idempotent.
func (d *Directory) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	for _, e := range d.sessions {
		d.endLocked(ctx, e, reasonShutdown)
	}
	return nil
}

// admitLocked rejects an Open that the directory cannot hold, after ending
// every idle session. A session of s that Open will evict makes room.
func (d *Directory) admitLocked(s scope) error {
	if d.closed {
		return ErrClosed
	}
	if len(d.sessions) < d.config.MaxSessions {
		return nil
	}
	now := d.config.Clock.Now()
	for id := range d.sessions {
		d.liveLocked(id, now)
	}
	evicted := 0
	if len(d.accounts[s]) >= d.config.MaxPerAccount {
		evicted = 1
	}
	if len(d.sessions)-evicted >= d.config.MaxSessions {
		return ErrFull
	}
	return nil
}

// liveLocked returns the session behind id, ending it first if its idle
// deadline has been reached or its lease was cut, so an idle end is exact
// even before its timer runs.
func (d *Directory) liveLocked(id string, now time.Time) *entry {
	e := d.sessions[id]
	switch {
	case e == nil:
		return nil
	case !now.Before(e.deadline):
		d.endLocked(e.ctx, e, reasonIdle)
		return nil
	case e.lease.Err() != nil:
		d.endLocked(e.ctx, e, reasonRevoked)
		return nil
	}
	return e
}

// renewLocked moves the idle deadline to Idle after now.
func (d *Directory) renewLocked(ctx context.Context, e *entry, now time.Time) {
	if e.timer != nil {
		e.timer.Stop()
	}
	e.ctx = context.WithoutCancel(ctx)
	e.deadline = now.Add(d.config.Idle)
	e.timer = d.config.Clock.AfterFunc(d.config.Idle, func() { d.expire(e) })
}

func (d *Directory) expire(e *entry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions[e.id] == e {
		d.liveLocked(e.id, d.config.Clock.Now())
	}
}

// endLocked forgets a known session and disconnects it.
func (d *Directory) endLocked(ctx context.Context, e *entry, why reason) {
	delete(d.sessions, e.id)
	d.accounts[e.scope] = slices.DeleteFunc(d.accounts[e.scope], func(other *entry) bool { return other == e })
	if len(d.accounts[e.scope]) == 0 {
		delete(d.accounts, e.scope)
	}
	e.timer.Stop()
	d.discard(e)
	d.config.Logger.InfoContext(ctx, "lock session ended",
		slog.String("reason", string(why)),
		slog.String("tenant_id", e.scope.tenantID))
}

// discard frees the session's locks at once and releases its registry
// lease, which would end the locking session through its context anyway.
func (d *Directory) discard(e *entry) {
	e.session.Detach()
	e.lease.Release()
}

// conn is the registry's view of one lock session. A cut or a registry
// shutdown ends the session before the cut returns; nothing is written to
// the network. The registry passes no request values to Close, so the end
// logs the correlation of the session's last Open or Use; the registry's
// own records carry the cut's.
type conn struct {
	directory *Directory
	entry     *entry
}

func (c conn) Close(_ context.Context, why revocation.Reason) error {
	if why == revocation.ReasonShutdown {
		c.end(reasonShutdown)
	} else {
		c.end(reasonRevoked)
	}
	return nil
}

func (c conn) Abort() { c.end(reasonRevoked) }

func (c conn) end(why reason) {
	d := c.directory
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions[c.entry.id] == c.entry {
		d.endLocked(c.entry.ctx, c.entry, why)
	}
}

// newID returns an opaque lock session ID: the prefix and 128 random bits.
func newID() string {
	var b [16]byte
	rand.Read(b[:]) // never returns an error since Go 1.24
	return idPrefix + hex.EncodeToString(b[:])
}
