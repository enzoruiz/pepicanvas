package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/locksession"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// lockHeartbeatInterval is how often the editor sends a heartbeat: well
// inside the 5 s a lease and its lock session live without one.
const lockHeartbeatInterval = 2 * time.Second

// lockSessionID is the shape of every lock session ID the directory issues.
var lockSessionID = regexp.MustCompile(`^pls_[0-9a-f]{32}$`)

var errLocksUnavailable = errors.New("object locks are not configured")

// LockSessions holds the lock sessions; *locksession.Directory implements it.
type LockSessions interface {
	Open(ctx context.Context, grant revocation.Grant, name string) (string, error)
	Use(ctx context.Context, id, tenantID, accountID string, fn func(*locking.Session) error) error
	End(ctx context.Context, id, tenantID, accountID string)
	Name(sessionID string) (string, bool)
}

// LockLeases lists a canvas's current locks; *locking.Manager implements it.
type LockLeases interface {
	Leases(tenantID, canvasID string) []locking.Lease
}

// MemberGrants reads an account's membership with its tenant's confirmed
// access version in one read; *postgres.AccountStore implements it.
type MemberGrants interface {
	MemberGrant(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, uint64, bool, error)
}

// Locks serves the temporary object locks (US-020).
type Locks struct {
	Directory LockSessions
	Manager   LockLeases
	Grants    MemberGrants
	// Now must tell the locking manager's time; nil uses time.Now.
	Now func() time.Time
}

func (l *Locks) validate() error {
	if l != nil && (l.Directory == nil || l.Manager == nil || l.Grants == nil) {
		return errors.New("httpapi: locks need a directory, a manager, and grants")
	}
	return nil
}

func (l *Locks) now() time.Time {
	if l.Now == nil {
		return time.Now()
	}
	return l.Now()
}

// LockAuthorizer is the locking.Authorizer of the lock manager: the holder
// must still be allowed to edit the canvas of the key, read from memberships
// on every acquire and for every lease on every heartbeat, so a renewal
// fails closed once access is lost (US-020 S10).
func LockAuthorizer(memberships access.MembershipStore) locking.Authorizer {
	return func(ctx context.Context, holder locking.Holder, key locking.Key) error {
		membership, found, err := memberships.MembershipOf(ctx, access.AccountID(holder.AccountID), key.TenantID)
		if err != nil {
			return fmt.Errorf("reauthorize lock: %w", err)
		}
		return access.Check(access.CanvasRequest(membership, found, key.TenantID, access.ActionEdit))
	}
}

type lockSessionResult struct {
	LockSessionID       string `json:"lock_session_id"`
	HeartbeatIntervalMS int64  `json:"heartbeat_interval_ms"`
	TTLMS               int64  `json:"ttl_ms"`
}

// lockHolder names a lock's holder by its account's display name only.
type lockHolder struct {
	Name string `json:"name"`
	Mine bool   `json:"mine"`
}

type lockRenewal struct {
	ObjectID    string `json:"object_id"`
	ExpiresInMS int64  `json:"expires_in_ms"`
}

type lockLoss struct {
	ObjectID string `json:"object_id"`
	Reason   string `json:"reason"`
}

type heartbeatResult struct {
	Renewed []lockRenewal `json:"renewed"`
	Lost    []lockLoss    `json:"lost"`
}

type acquireBody struct {
	OperationID string `json:"operation_id"`
	ObjectID    string `json:"object_id"`
}

type acquireResult struct {
	OperationID string     `json:"operation_id"`
	Outcome     string     `json:"outcome"`
	ObjectID    string     `json:"object_id"`
	Holder      lockHolder `json:"holder"`
	ExpiresInMS int64      `json:"expires_in_ms"`
}

type releaseBody struct {
	OperationID string `json:"operation_id"`
	ObjectID    string `json:"object_id"`
	Reason      string `json:"reason"`
}

type releaseResult struct {
	OperationID string `json:"operation_id"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason"`
	ObjectID    string `json:"object_id"`
}

type lockEntry struct {
	ObjectID string     `json:"object_id"`
	Holder   lockHolder `json:"holder"`
}

type lockList struct {
	Locks []lockEntry `json:"locks"`
}

// openLockSession opens a lock session of the caller on the canvas's
// tenant, bound to a registry lease read at the tenant's access version, so
// a later cut ends it. The ingress checks Origin and the CSRF token before
// the session is read; the edit access is checked before and again on the
// facts the grant is read with.
func (a *api) openLockSession(w http.ResponseWriter, r *http.Request) {
	token, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionEdit)
	if !ok {
		return
	}
	if a.Locks == nil {
		a.responder.Error(w, r, httpx.ErrLockUnavailable, errLocksUnavailable)
		return
	}
	membership, version, found, err := a.Locks.Grants.MemberGrant(r.Context(), principal.AccountID, key.TenantID)
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	if err := access.Check(access.CanvasRequest(membership, found, key.TenantID, access.ActionEdit)); err != nil {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
		return
	}
	// The registry names the auth session by its token hash, the
	// sessions.token_hash key, never by the token.
	hash := sha256.Sum256([]byte(token.Reveal()))
	id, err := a.Locks.Directory.Open(r.Context(), revocation.Grant{
		TenantID:  key.TenantID,
		AccountID: string(principal.AccountID),
		SessionID: hex.EncodeToString(hash[:]),
		Client:    revocation.ClientEditor,
		Version:   version,
	}, principal.Email)
	if errors.Is(err, locking.ErrDetached) {
		// A cut reached the new lease before its locking session attached.
		err = revocation.ErrAccessRevoked
	}
	if err != nil {
		a.lockError(w, r, principal, key, "", err)
		return
	}
	writeJSON(w, http.StatusCreated, lockSessionResult{
		LockSessionID:       id,
		HeartbeatIntervalMS: lockHeartbeatInterval.Milliseconds(),
		TTLMS:               locking.DefaultTTL.Milliseconds(),
	})
}

// heartbeatLock renews every lock the session still holds and reports the
// ones it lost since its previous heartbeat.
func (a *api) heartbeatLock(w http.ResponseWriter, r *http.Request) {
	principal, key, id, ok := a.lockSession(w, r)
	if !ok {
		return
	}
	var renewal locking.Renewal
	err := a.Locks.Directory.Use(r.Context(), id, key.TenantID, string(principal.AccountID), func(s *locking.Session) error {
		var err error
		renewal, err = s.Heartbeat(r.Context())
		return err
	})
	if err != nil {
		a.lockError(w, r, principal, key, id, err)
		return
	}
	now := a.Locks.now()
	body := heartbeatResult{Renewed: make([]lockRenewal, 0, len(renewal.Renewed)), Lost: make([]lockLoss, 0, len(renewal.Lost))}
	for _, lease := range renewal.Renewed {
		body.Renewed = append(body.Renewed, lockRenewal{ObjectID: lease.Key.ObjectID, ExpiresInMS: expiresIn(lease, now)})
	}
	for _, loss := range renewal.Lost {
		body.Lost = append(body.Lost, lockLoss{ObjectID: loss.Key.ObjectID, Reason: string(loss.Reason)})
	}
	writeJSON(w, http.StatusOK, body)
}

// acquireLock locks one object of the canvas of the path for the session,
// or names its current holder. A retry under the same operation ID, also
// from a later session of the same account, repeats the answer without a
// second effect (CTRL-IDEM-01); it does not mean the lock is still held.
func (a *api) acquireLock(w http.ResponseWriter, r *http.Request) {
	principal, key, id, ok := a.lockSession(w, r)
	if !ok {
		return
	}
	var body acquireBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	var result locking.Result
	err := a.Locks.Directory.Use(r.Context(), id, key.TenantID, string(principal.AccountID), func(s *locking.Session) error {
		var err error
		result, err = s.Acquire(r.Context(), locking.Request{OperationID: body.OperationID, CanvasID: key.CanvasID, ObjectID: body.ObjectID})
		return err
	})
	if err != nil {
		a.lockError(w, r, principal, key, id, err)
		return
	}
	writeJSON(w, http.StatusOK, acquireResult{
		OperationID: result.OperationID,
		Outcome:     string(result.Outcome),
		ObjectID:    result.Lease.Key.ObjectID,
		Holder:      a.lockHolder(principal, result.Lease.Holder),
		ExpiresInMS: expiresIn(result.Lease, a.Locks.now()),
	})
}

// releaseLock frees a lock the session holds at once, as finished or
// cancelled. A retry under the same operation ID repeats the answer.
func (a *api) releaseLock(w http.ResponseWriter, r *http.Request) {
	principal, key, id, ok := a.lockSession(w, r)
	if !ok {
		return
	}
	var body releaseBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	var result locking.Result
	err := a.Locks.Directory.Use(r.Context(), id, key.TenantID, string(principal.AccountID), func(s *locking.Session) error {
		var err error
		result, err = s.Release(r.Context(), locking.Request{
			OperationID: body.OperationID,
			CanvasID:    key.CanvasID,
			ObjectID:    body.ObjectID,
			Reason:      locking.Reason(body.Reason),
		})
		return err
	})
	if err != nil {
		a.lockError(w, r, principal, key, id, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseResult{
		OperationID: result.OperationID,
		Outcome:     string(result.Outcome),
		Reason:      string(result.Reason),
		ObjectID:    result.Lease.Key.ObjectID,
	})
}

// endLockSession disconnects one of the caller's lock sessions, freeing its
// locks at once. Ending needs no access: it only frees, the directory
// ignores IDs of another tenant or account, and it is idempotent.
func (a *api) endLockSession(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if a.Locks == nil {
		a.responder.Error(w, r, httpx.ErrLockUnavailable, errLocksUnavailable)
		return
	}
	a.Locks.Directory.End(r.Context(), r.PathValue("sessionID"), r.PathValue("tenantID"), string(principal.AccountID))
	w.WriteHeader(http.StatusNoContent)
}

// listLocks answers the current locks of a canvas the caller may view, each
// with its holder's display name. No identifier of a holder is exposed.
func (a *api) listLocks(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionView)
	if !ok {
		return
	}
	if a.Locks == nil {
		a.responder.Error(w, r, httpx.ErrLockUnavailable, errLocksUnavailable)
		return
	}
	leases := a.Locks.Manager.Leases(key.TenantID, key.CanvasID)
	body := lockList{Locks: make([]lockEntry, 0, len(leases))}
	for _, lease := range leases {
		body.Locks = append(body.Locks, lockEntry{ObjectID: lease.Key.ObjectID, Holder: a.lockHolder(principal, lease.Holder)})
	}
	writeJSON(w, http.StatusOK, body)
}

// lockSession authenticates the caller and reauthorizes its edit access to
// the canvas of the path before any lock action (US-020 S10), with the
// check of authorizeCanvas. A caller denied access has the lock session of
// the path ended first, so its locks are free before the shared forbidden
// answer is sent (S8). It then checks the lock service is serving and the
// lock session ID has the shape the directory issues.
func (a *api) lockSession(w http.ResponseWriter, r *http.Request) (access.Principal, canvas.Key, string, bool) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return access.Principal{}, canvas.Key{}, "", false
	}
	key := canvas.Key{TenantID: r.PathValue("tenantID"), CanvasID: r.PathValue("canvasID")}
	id := r.PathValue("sessionID")
	denied := canvas.ValidateKey(key)
	if denied == nil {
		membership, found, err := a.Memberships.MembershipOf(r.Context(), principal.AccountID, key.TenantID)
		if err != nil {
			a.responder.Error(w, r, httpx.ErrInternal, err)
			return access.Principal{}, canvas.Key{}, "", false
		}
		denied = access.Check(access.CanvasRequest(membership, found, key.TenantID, access.ActionEdit))
	}
	if denied != nil {
		a.lockError(w, r, principal, key, id, access.ErrAccessDenied)
		return access.Principal{}, canvas.Key{}, "", false
	}
	if a.Locks == nil {
		a.responder.Error(w, r, httpx.ErrLockUnavailable, errLocksUnavailable)
		return access.Principal{}, canvas.Key{}, "", false
	}
	if !lockSessionID.MatchString(id) {
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("malformed lock session ID"))
		return access.Principal{}, canvas.Key{}, "", false
	}
	return principal, key, id, true
}

// lockError maps a lock error to its public answer. A denial ends the lock
// session id, if any, before answering, so its locks are already free.
func (a *api) lockError(w http.ResponseWriter, r *http.Request, principal access.Principal, key canvas.Key, id string, err error) {
	switch {
	case errors.Is(err, access.ErrAccessDenied), errors.Is(err, revocation.ErrAccessRevoked):
		if id != "" && a.Locks != nil {
			a.Locks.Directory.End(r.Context(), id, key.TenantID, string(principal.AccountID))
		}
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	case errors.Is(err, locksession.ErrGone), errors.Is(err, locking.ErrDetached):
		// Its locks are free and never restored; the client opens a new
		// session (S7).
		a.responder.Error(w, r, httpx.ErrLockSessionGone, err)
	case errors.Is(err, locking.ErrNotHolder):
		a.responder.Error(w, r, httpx.ErrNotLockHolder, err)
	case errors.Is(err, locking.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	case errors.Is(err, locking.ErrInvalidRequest):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, locking.ErrClosed), errors.Is(err, locksession.ErrClosed), errors.Is(err, locksession.ErrFull),
		errors.Is(err, revocation.ErrClosed), errors.Is(err, revocation.ErrStaleGrant):
		a.responder.Error(w, r, httpx.ErrLockUnavailable, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// lockHolder names holder to the caller: by the caller's own email when it
// is the caller's account, so a retried answer from an ended session reads
// the same, and otherwise by the display name of its lock session, empty
// once that session ended.
func (a *api) lockHolder(principal access.Principal, holder locking.Holder) lockHolder {
	if holder.AccountID == string(principal.AccountID) {
		return lockHolder{Name: principal.Email, Mine: true}
	}
	name, _ := a.Locks.Directory.Name(holder.SessionID)
	return lockHolder{Name: name}
}

// expiresIn is the time left on lease at now, never negative.
func expiresIn(lease locking.Lease, now time.Time) int64 {
	return max(lease.Expires.Sub(now).Milliseconds(), 0)
}
