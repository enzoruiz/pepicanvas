// Package httpapi serves the account, session, and canvas-request HTTP API
// (US-001), the manual streamer activation and own-canvas read (US-002), the
// operator deactivation (US-003), the owner's moderator invitations (US-004),
// their acceptance and the moderated-canvas list (US-005), the owner-only
// canvas resize (US-007), and the canvas storage quota with its media
// uploads and references (US-016). Every response is private and
// correlated, and every route is guarded by the ingress (TE-014): unsafe
// methods need the exact Origin, session mutations need the session's CSRF
// token before the session is read, and the abuse limits answer 429
// (CTRL-WEB-01, CTRL-RATE-01). Invitation tokens arrive only in request bodies
// and never appear in a response or a log line.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const (
	// SessionCookie holds the session token. The __Host- prefix makes the
	// browser require Secure, Path=/, and no Domain.
	SessionCookie = ingress.SessionCookie
	// CSRFHeader carries the session's CSRF token on authenticated mutations.
	CSRFHeader = ingress.CSRFHeader

	maxBodyBytes = 4096
	// maxLimiterKeys bounds each limiter's memory; a full table fails closed.
	maxLimiterKeys = 100_000
)

// Rate is a number of attempts allowed per window.
type Rate = ratelimit.Rate

// Limits are the abuse limits of the sign-in, registration, invitation
// creation and acceptance, and media upload routes, answered through the
// ingress. Addresses come from the TCP peer; invitation creation, acceptance,
// and uploads are keyed by the session's account.
type Limits struct {
	SignInPerAccount      Rate
	SignInPerIP           Rate
	RegisterPerIP         Rate
	InvitationsPerAccount Rate
	// InvitationAcceptsPerAccount limits acceptances by the session's
	// account, before the body is read.
	InvitationAcceptsPerAccount Rate
	UploadsPerAccount           Rate
}

// DefaultLimits are the pilot limits.
var DefaultLimits = Limits{
	SignInPerAccount:            Rate{Limit: 10, Window: 15 * time.Minute},
	SignInPerIP:                 Rate{Limit: 50, Window: 15 * time.Minute},
	RegisterPerIP:               Rate{Limit: 10, Window: time.Hour},
	InvitationsPerAccount:       Rate{Limit: 20, Window: time.Hour},
	InvitationAcceptsPerAccount: Rate{Limit: 20, Window: time.Hour},
	UploadsPerAccount:           Rate{Limit: 60, Window: time.Hour},
}

// Accounts is the account and session service.
type Accounts interface {
	Register(ctx context.Context, request access.RegisterRequest) (access.AccountID, error)
	SignIn(ctx context.Context, email string, password secret.Value) (access.Session, error)
	Authenticate(ctx context.Context, token secret.Value) (access.Principal, error)
	SignOut(ctx context.Context, token secret.Value) error
}

// CanvasStore reads committed canvases and commits canvas mutations.
type CanvasStore interface {
	Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error)
	Apply(ctx context.Context, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (canvas.Result, error)
}

// Subscriptions is the streamer activation and deactivation service.
type Subscriptions interface {
	Activate(ctx context.Context, request subscription.ActivateRequest) (subscription.Activation, error)
	Deactivate(ctx context.Context, request subscription.DeactivateRequest) (subscription.Deactivation, error)
	CanvasOf(ctx context.Context, account access.AccountID) (canvas.Key, error)
	IsOperator(ctx context.Context, account access.AccountID) (bool, error)
}

// Tenants reads the tenant an account owns. A denied own-canvas read uses it
// to tell the owner of a deactivated tenant why, and nobody else.
type Tenants interface {
	OwnedTenant(ctx context.Context, account access.AccountID) (subscription.Tenant, bool, error)
}

// Invitations is the moderator invitation service.
type Invitations interface {
	Create(ctx context.Context, request invitation.CreateRequest) (invitation.Invitation, error)
	List(ctx context.Context, owner access.AccountID) ([]invitation.Invitation, error)
	Revoke(ctx context.Context, request invitation.RevokeRequest) (invitation.Revocation, error)
	Accept(ctx context.Context, request invitation.AcceptRequest) (invitation.Acceptance, error)
	Moderations(ctx context.Context, account access.AccountID) ([]invitation.Moderation, error)
}

// Config wires the API.
type Config struct {
	Accounts      Accounts
	Memberships   access.MembershipStore
	Canvases      CanvasStore
	Subscriptions Subscriptions
	Tenants       Tenants
	Invitations   Invitations
	// Media holds the canvas quotas and assets. Without it every media route
	// answers media_unavailable.
	Media media.Store
	// Storage and Inspector store and validate uploaded bytes. Without either
	// the upload route answers media_unavailable before reserving quota.
	Storage   media.Storage
	Inspector media.Inspector
	// Ingress guards every route; it holds the allowed origins.
	Ingress *ingress.Ingress
	Logger  *slog.Logger
	// Now is the limiter clock; nil uses time.Now.
	Now    func() time.Time
	Limits Limits
}

type api struct {
	Config
	responder             httpx.Responder
	signInPerAccount      *ratelimit.Limiter
	signInPerIP           *ratelimit.Limiter
	registerPerIP         *ratelimit.Limiter
	invitationsPerAccount *ratelimit.Limiter
	acceptsPerAccount     *ratelimit.Limiter
	uploadsPerAccount     *ratelimit.Limiter
}

// New validates the configuration and returns the API handler.
func New(config Config) (http.Handler, error) {
	if config.Ingress == nil {
		return nil, errors.New("httpapi: ingress is required")
	}
	if config.Accounts == nil || config.Memberships == nil || config.Canvases == nil || config.Subscriptions == nil || config.Tenants == nil || config.Invitations == nil {
		return nil, errors.New("httpapi: accounts, memberships, canvases, subscriptions, tenants, and invitations are required")
	}
	for _, rate := range []Rate{config.Limits.SignInPerAccount, config.Limits.SignInPerIP, config.Limits.RegisterPerIP, config.Limits.InvitationsPerAccount, config.Limits.InvitationAcceptsPerAccount, config.Limits.UploadsPerAccount} {
		if rate.Limit < 1 || rate.Window <= 0 {
			return nil, errors.New("httpapi: every limit needs a positive count and window")
		}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	limiter := func(rate Rate) *ratelimit.Limiter {
		return ratelimit.New(rate.Limit, rate.Window, maxLimiterKeys, config.Now)
	}
	a := &api{
		Config:           config,
		responder:        httpx.Responder{Logger: config.Logger},
		signInPerAccount: limiter(config.Limits.SignInPerAccount),
		signInPerIP:      limiter(config.Limits.SignInPerIP),
		registerPerIP:    limiter(config.Limits.RegisterPerIP),
		// Invitation creation is keyed by account ID, never by email.
		invitationsPerAccount: limiter(config.Limits.InvitationsPerAccount),
		acceptsPerAccount:     limiter(config.Limits.InvitationAcceptsPerAccount),
		uploadsPerAccount:     limiter(config.Limits.UploadsPerAccount),
	}

	mux := http.NewServeMux()
	in := config.Ingress
	mux.Handle("POST /api/accounts", in.Route(ingress.Anonymous, http.HandlerFunc(a.register)))
	mux.Handle("POST /api/sessions", in.Route(ingress.Anonymous, http.HandlerFunc(a.signIn)))
	mux.HandleFunc("GET /api/session", a.currentSession)
	mux.Handle("DELETE /api/session", in.Route(ingress.Session, http.HandlerFunc(a.signOut)))
	mux.HandleFunc("GET /api/tenants/{tenantID}/canvases/{canvasID}", a.canvas)
	mux.Handle("PUT /api/tenants/{tenantID}/canvases/{canvasID}/dimensions", in.Route(ingress.Session, http.HandlerFunc(a.setDimensions)))
	mux.HandleFunc("GET /api/tenants/{tenantID}/canvases/{canvasID}/media/usage", a.mediaUsage)
	mux.Handle("POST /api/tenants/{tenantID}/canvases/{canvasID}/media/uploads", in.Route(ingress.Session, http.HandlerFunc(a.uploadMedia)))
	mux.Handle("POST /api/tenants/{tenantID}/canvases/{canvasID}/media/references", in.Route(ingress.Session, http.HandlerFunc(a.attachMedia)))
	mux.Handle("POST /api/tenants/{tenantID}/canvases/{canvasID}/media/references/{referenceID}/detach", in.Route(ingress.Session, http.HandlerFunc(a.detachMedia)))
	mux.Handle("POST /api/activations", in.Route(ingress.Session, http.HandlerFunc(a.activate)))
	mux.Handle("POST /api/deactivations", in.Route(ingress.Session, http.HandlerFunc(a.deactivate)))
	mux.HandleFunc("GET /api/canvas", a.ownCanvas)
	mux.Handle("POST /api/invitations", in.Route(ingress.Session, http.HandlerFunc(a.createInvitation)))
	mux.HandleFunc("GET /api/invitations", a.listInvitations)
	mux.Handle("POST /api/invitations/{invitationID}/revoke", in.Route(ingress.Session, http.HandlerFunc(a.revokeInvitation)))
	mux.Handle("POST /api/invitations/accept", in.Route(ingress.Session, http.HandlerFunc(a.acceptInvitation)))
	mux.HandleFunc("GET /api/memberships", a.listMemberships)
	return httpx.Recover(config.Logger, httpx.Correlate(httpx.Private(in.Protect(mux)))), nil
}

type registerBody struct {
	OperationID string `json:"operation_id"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

func (a *api) register(w http.ResponseWriter, r *http.Request) {
	if !a.Ingress.Allow(w, r, a.registerPerIP, ingress.ClientIP(r), ingress.ReasonRateIP) {
		return
	}
	var body registerBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	id, err := a.Accounts.Register(r.Context(), access.RegisterRequest{
		OperationID: body.OperationID,
		Email:       body.Email,
		Password:    secret.New(secret.KindPassword, body.Password),
	})
	switch {
	case err == nil:
		a.Logger.InfoContext(r.Context(), "account registered", slog.String("account_id", string(id)))
		writeJSON(w, http.StatusCreated, map[string]string{"account_id": string(id)})
	case errors.Is(err, access.ErrInvalidRegistration):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, access.ErrIdentityTaken):
		a.responder.Error(w, r, httpx.ErrIdentityTaken, err)
	case errors.Is(err, access.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

type credentialsBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type sessionBody struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	CSRFToken string `json:"csrf_token"`
	Operator  bool   `json:"operator"`
}

func (a *api) signIn(w http.ResponseWriter, r *http.Request) {
	var body credentialsBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	if !a.Ingress.Allow(w, r, a.signInPerIP, ingress.ClientIP(r), ingress.ReasonRateIP) ||
		!a.Ingress.Allow(w, r, a.signInPerAccount, accountKey(body.Email), ingress.ReasonRateAccount) {
		return
	}
	session, err := a.Accounts.SignIn(r.Context(), body.Email, secret.New(secret.KindPassword, body.Password))
	if errors.Is(err, access.ErrInvalidCredentials) {
		a.responder.Error(w, r, httpx.ErrUnauthorized, err)
		return
	}
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	// A successful sign-in replaces the session the browser presented, so an
	// earlier session never outlives the cookie that carried it.
	if presented := sessionToken(r); !presented.IsZero() {
		if err := a.Accounts.SignOut(r.Context(), presented); err != nil {
			a.responder.Error(w, r, httpx.ErrInternal, err)
			return
		}
	}
	principal, err := a.Accounts.Authenticate(r.Context(), session.Token)
	if err != nil {
		a.abandonSession(r, session.Token)
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	operator, err := a.Subscriptions.IsOperator(r.Context(), principal.AccountID)
	if err != nil {
		a.abandonSession(r, session.Token)
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    session.Token.Reveal(),
		Path:     "/",
		MaxAge:   int(access.SessionAbsoluteLifetime / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	a.Logger.InfoContext(r.Context(), "session created", slog.String("account_id", string(session.AccountID)))
	writeJSON(w, http.StatusCreated, sessionBody{
		AccountID: string(principal.AccountID),
		Email:     principal.Email,
		CSRFToken: session.CSRF.Reveal(),
		Operator:  operator,
	})
}

// abandonSession ends a session whose token the browser never received, so a
// failed sign-in leaves no usable server-side session. It is best-effort: a
// failure is logged and the session expires on its own.
func (a *api) abandonSession(r *http.Request, token secret.Value) {
	if err := a.Accounts.SignOut(r.Context(), token); err != nil {
		a.Logger.ErrorContext(r.Context(), "undelivered session not ended", slog.String("error", err.Error()))
	}
}

func (a *api) currentSession(w http.ResponseWriter, r *http.Request) {
	token, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	operator, err := a.Subscriptions.IsOperator(r.Context(), principal.AccountID)
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionBody{
		AccountID: string(principal.AccountID),
		Email:     principal.Email,
		CSRFToken: access.CSRFToken(token).Reveal(),
		Operator:  operator,
	})
}

type activationBody struct {
	OperationID string `json:"operation_id"`
	Email       string `json:"email"`
}

type activationResult struct {
	AccountID string `json:"account_id"`
	TenantID  string `json:"tenant_id"`
	CanvasID  string `json:"canvas_id"`
	Created   bool   `json:"created"`
}

// activate lets an operator activate the account of an email. The ingress
// checks the CSRF token before the session is read, and the session is
// checked before the body is read; the operator role is checked by the domain
// before the account is looked up.
func (a *api) activate(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	var body activationBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	result, err := a.Subscriptions.Activate(r.Context(), subscription.ActivateRequest{
		OperatorID:    principal.AccountID,
		OperationID:   body.OperationID,
		Email:         body.Email,
		CorrelationID: correlationID.String(),
	})
	switch {
	case err == nil:
		a.Logger.InfoContext(r.Context(), "activation confirmed",
			slog.String("account_id", string(result.AccountID)),
			slog.String("tenant_id", result.TenantID),
			slog.String("canvas_id", result.CanvasID),
		)
		writeJSON(w, http.StatusOK, activationResult{
			AccountID: string(result.AccountID),
			TenantID:  result.TenantID,
			CanvasID:  result.CanvasID,
			Created:   result.Created,
		})
	case errors.Is(err, access.ErrAccessDenied):
		a.responder.Error(w, r, httpx.ErrForbidden, err)
	case errors.Is(err, subscription.ErrInvalidActivation):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, subscription.ErrAccountNotFound):
		a.responder.Error(w, r, httpx.ErrNotFound, err)
	case errors.Is(err, subscription.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

type deactivationResult struct {
	AccountID   string `json:"account_id"`
	TenantID    string `json:"tenant_id"`
	CanvasID    string `json:"canvas_id"`
	Deactivated bool   `json:"deactivated"`
}

// deactivate lets an operator cut the access of the tenant owned by the
// account of an email; reactivation is activate. The ingress checks Origin
// and the CSRF token before the session is read, and the rest runs in the
// order of activate. The cut is effective when the answer is sent; the access
// version stays internal.
func (a *api) deactivate(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	var body activationBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	result, err := a.Subscriptions.Deactivate(r.Context(), subscription.DeactivateRequest{
		OperatorID:    principal.AccountID,
		OperationID:   body.OperationID,
		Email:         body.Email,
		CorrelationID: correlationID.String(),
	})
	switch {
	case err == nil:
		a.Logger.InfoContext(r.Context(), "deactivation confirmed",
			slog.String("account_id", string(result.AccountID)),
			slog.String("tenant_id", result.TenantID),
			slog.String("canvas_id", result.CanvasID),
		)
		writeJSON(w, http.StatusOK, deactivationResult{
			AccountID:   string(result.AccountID),
			TenantID:    result.TenantID,
			CanvasID:    result.CanvasID,
			Deactivated: result.Deactivated,
		})
	case errors.Is(err, access.ErrAccessDenied):
		a.responder.Error(w, r, httpx.ErrForbidden, err)
	case errors.Is(err, subscription.ErrInvalidDeactivation):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, subscription.ErrAccountNotFound):
		a.responder.Error(w, r, httpx.ErrNotFound, err)
	case errors.Is(err, subscription.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

type invitationBody struct {
	OperationID string `json:"operation_id"`
	Token       string `json:"token"`
}

type invitationResult struct {
	InvitationID string `json:"invitation_id"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
}

type invitationList struct {
	Invitations []invitationResult `json:"invitations"`
}

type revocationBody struct {
	OperationID string `json:"operation_id"`
}

type revocationResult struct {
	InvitationID string `json:"invitation_id"`
	Status       string `json:"status"`
}

func newInvitationResult(i invitation.Invitation) invitationResult {
	return invitationResult{
		InvitationID: i.ID,
		CreatedAt:    i.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:    i.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

// invitationError maps an invitation service error to its public answer.
// Unknown and other-tenant invitation IDs share the one not-found answer.
func (a *api) invitationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, access.ErrAccessDenied):
		a.responder.Error(w, r, httpx.ErrForbidden, err)
	case errors.Is(err, invitation.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, invitation.ErrNotFound):
		a.responder.Error(w, r, httpx.ErrNotFound, err)
	case errors.Is(err, invitation.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// createInvitation stores the hash of a client-generated token as an
// invitation to the session owner's tenant. The creation limit is checked
// after the session and before the body is read; the ingress has already
// checked the Origin and the CSRF token.
func (a *api) createInvitation(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.Ingress.Allow(w, r, a.invitationsPerAccount, string(principal.AccountID), ingress.ReasonRateAccount) {
		return
	}
	var body invitationBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	created, err := a.Invitations.Create(r.Context(), invitation.CreateRequest{
		OwnerID:       principal.AccountID,
		OperationID:   body.OperationID,
		Token:         secret.New(secret.KindInvitationToken, body.Token),
		CorrelationID: correlationID.String(),
	})
	if err != nil {
		a.invitationError(w, r, err)
		return
	}
	a.Logger.InfoContext(r.Context(), "invitation created",
		slog.String("tenant_id", created.TenantID),
		slog.String("invitation_id", created.ID),
	)
	writeJSON(w, http.StatusCreated, newInvitationResult(created))
}

// listInvitations answers the session owner's pending invitations.
func (a *api) listInvitations(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	pending, err := a.Invitations.List(r.Context(), principal.AccountID)
	if err != nil {
		a.invitationError(w, r, err)
		return
	}
	body := invitationList{Invitations: make([]invitationResult, 0, len(pending))}
	for _, i := range pending {
		body.Invitations = append(body.Invitations, newInvitationResult(i))
	}
	writeJSON(w, http.StatusOK, body)
}

// revokeInvitation revokes an invitation of the session owner's tenant. The
// ingress has already checked the Origin and the CSRF token.
func (a *api) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	var body revocationBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	revoked, err := a.Invitations.Revoke(r.Context(), invitation.RevokeRequest{
		OwnerID:       principal.AccountID,
		OperationID:   body.OperationID,
		InvitationID:  r.PathValue("invitationID"),
		CorrelationID: correlationID.String(),
	})
	if err != nil {
		a.invitationError(w, r, err)
		return
	}
	a.Logger.InfoContext(r.Context(), "invitation revoked",
		slog.String("invitation_id", revoked.InvitationID),
		slog.String("status", string(revoked.Status)),
	)
	writeJSON(w, http.StatusOK, revocationResult{InvitationID: revoked.InvitationID, Status: string(revoked.Status)})
}

type acceptanceResult struct {
	TenantID string `json:"tenant_id"`
	CanvasID string `json:"canvas_id"`
}

type moderationResult struct {
	TenantID string `json:"tenant_id"`
	CanvasID string `json:"canvas_id"`
	Role     string `json:"role"`
}

type moderationList struct {
	Memberships []moderationResult `json:"memberships"`
}

// acceptInvitation makes the session's account a moderator of the tenant of
// the token in the body. The ingress has already checked the Origin and the
// CSRF token; the acceptance limit is checked after the session and before
// the body is read. Every unusable link gets the one not-found answer.
func (a *api) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !a.Ingress.Allow(w, r, a.acceptsPerAccount, string(principal.AccountID), ingress.ReasonRateAccount) {
		return
	}
	var body invitationBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	accepted, err := a.Invitations.Accept(r.Context(), invitation.AcceptRequest{
		AccountID:     principal.AccountID,
		OperationID:   body.OperationID,
		Token:         secret.New(secret.KindInvitationToken, body.Token),
		CorrelationID: correlationID.String(),
	})
	switch {
	case err == nil:
		a.Logger.InfoContext(r.Context(), "invitation accepted",
			slog.String("tenant_id", accepted.TenantID),
			slog.String("invitation_id", accepted.InvitationID),
		)
		writeJSON(w, http.StatusOK, acceptanceResult{TenantID: accepted.TenantID, CanvasID: accepted.CanvasID})
	case errors.Is(err, invitation.ErrUnavailable):
		a.responder.Error(w, r, httpx.ErrNotFound, err)
	case errors.Is(err, invitation.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, invitation.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// listMemberships answers the canvases the session's account moderates in
// active tenants.
func (a *api) listMemberships(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	moderations, err := a.Invitations.Moderations(r.Context(), principal.AccountID)
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	body := moderationList{Memberships: make([]moderationResult, 0, len(moderations))}
	for _, m := range moderations {
		body.Memberships = append(body.Memberships, moderationResult{TenantID: m.TenantID, CanvasID: m.CanvasID, Role: string(m.Role)})
	}
	writeJSON(w, http.StatusOK, body)
}

// signOut ends the session. The ingress has already checked the CSRF token
// of a presented cookie, so only the session's own page can end it.
func (a *api) signOut(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	principal, err := a.Accounts.Authenticate(r.Context(), token)
	if errors.Is(err, access.ErrSessionInvalid) {
		// Nothing to protect: the session already ended.
		clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	if err := a.Accounts.SignOut(r.Context(), token); err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	clearSessionCookie(w)
	a.Logger.InfoContext(r.Context(), "session ended", slog.String("account_id", string(principal.AccountID)))
	w.WriteHeader(http.StatusNoContent)
}

type objectBody struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Content  string  `json:"content"`
	X        int     `json:"x"`
	Y        int     `json:"y"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Rotation float64 `json:"rotation"`
	Z        int64   `json:"z"`
}

type canvasBody struct {
	CanvasID string       `json:"canvas_id"`
	Width    int          `json:"width"`
	Height   int          `json:"height"`
	Revision int64        `json:"revision"`
	Objects  []objectBody `json:"objects"`
}

type ownCanvasBody struct {
	TenantID string `json:"tenant_id"`
	canvasBody
}

func newCanvasBody(loaded canvas.Canvas) canvasBody {
	body := canvasBody{
		CanvasID: loaded.Key.CanvasID,
		Width:    loaded.Dimensions.Width,
		Height:   loaded.Dimensions.Height,
		Revision: loaded.Revision,
		Objects:  make([]objectBody, 0, len(loaded.Objects)),
	}
	for _, o := range loaded.Objects {
		body.Objects = append(body.Objects, objectBody{
			ID: o.ID, Kind: string(o.Kind), Content: o.Content,
			X: o.Transform.X, Y: o.Transform.Y, Width: o.Transform.Width, Height: o.Transform.Height,
			Rotation: o.Transform.Rotation, Z: o.Z,
		})
	}
	return body
}

// ownCanvas answers the canvas of the tenant the account owns. The owner of
// a deactivated tenant is told its subscription is inactive; every other
// denial, including a missing canvas, is the forbidden answer of the canvas
// route.
func (a *api) ownCanvas(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, err := a.Subscriptions.CanvasOf(r.Context(), principal.AccountID)
	if errors.Is(err, access.ErrAccessDenied) {
		// Only the account's own tenant is read, so the answer reveals
		// nothing about any other tenant.
		tenant, owns, readErr := a.Tenants.OwnedTenant(r.Context(), principal.AccountID)
		switch {
		case readErr != nil:
			a.responder.Error(w, r, httpx.ErrInternal, readErr)
		case owns && tenant.Status == access.SubscriptionInactive:
			a.responder.Error(w, r, httpx.ErrSubscriptionInactive, err)
		default:
			a.responder.Error(w, r, httpx.ErrForbidden, err)
		}
		return
	}
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	loaded, ok := a.load(w, r, key)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ownCanvasBody{TenantID: key.TenantID, canvasBody: newCanvasBody(loaded)})
}

// load reads a canvas the caller may view. A missing or invalid canvas is
// the access denial.
func (a *api) load(w http.ResponseWriter, r *http.Request, key canvas.Key) (canvas.Canvas, bool) {
	loaded, err := a.Canvases.Load(r.Context(), key)
	if errors.Is(err, canvas.ErrNotFound) || errors.Is(err, canvas.ErrInvalid) {
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
		return canvas.Canvas{}, false
	}
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return canvas.Canvas{}, false
	}
	return loaded, true
}

// canvas answers a canvas request. Every denial, including a missing tenant
// or canvas, returns the same forbidden answer, so no response reveals
// whether a canvas exists for another tenant.
func (a *api) canvas(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionView)
	if !ok {
		return
	}
	loaded, ok := a.load(w, r, key)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newCanvasBody(loaded))
}

// authorizeCanvas reads the canvas key from the path and checks the caller
// may perform the action on it. Every denial is the same forbidden answer.
func (a *api) authorizeCanvas(w http.ResponseWriter, r *http.Request, principal access.Principal, action access.Action) (canvas.Key, bool) {
	key := canvas.Key{TenantID: r.PathValue("tenantID"), CanvasID: r.PathValue("canvasID")}
	if err := canvas.ValidateKey(key); err != nil {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
		return canvas.Key{}, false
	}
	membership, found, err := a.Memberships.MembershipOf(r.Context(), principal.AccountID, key.TenantID)
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return canvas.Key{}, false
	}
	if err := access.Check(access.CanvasRequest(membership, found, key.TenantID, action)); err != nil {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
		return canvas.Key{}, false
	}
	return key, true
}

// dimensionsBody decodes width and height as pointers, so a missing or null
// value is rejected rather than read as zero.
type dimensionsBody struct {
	OperationID string `json:"operation_id"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

type dimensionsResult struct {
	CanvasID string `json:"canvas_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Revision int64  `json:"revision"`
}

// setDimensions lets the owner resize the canvas. The ingress checks the
// CSRF token before the session is read, and the session and the owner check
// run before the body is read, so every other caller gets the same answer
// whatever it sends. The owner is reauthorized immediately before the change
// is applied. A retry under the same operation ID returns the same answer
// without a second effect.
func (a *api) setDimensions(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionAdminister)
	if !ok {
		return
	}
	var body dimensionsBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	if body.Width == nil || body.Height == nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("width and height are required"))
		return
	}
	correlationID, _ := correlation.FromContext(r.Context())
	operation := canvas.Operation{ID: body.OperationID, CorrelationID: correlationID.String()}
	mutation := canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: *body.Width, Height: *body.Height}}
	if err := canvas.Validate(key, operation, mutation); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}

	var (
		result  canvas.Result
		loadErr error
	)
	err := access.GuardMutation(func() (access.Request, error) {
		membership, found, err := a.Memberships.MembershipOf(r.Context(), principal.AccountID, key.TenantID)
		loadErr = err
		return access.CanvasRequest(membership, found, key.TenantID, access.ActionAdminister), err
	}, func() error {
		var err error
		result, err = a.Canvases.Apply(r.Context(), key, operation, mutation)
		return err
	})
	switch {
	case loadErr != nil:
		a.responder.Error(w, r, httpx.ErrInternal, loadErr)
	case err == nil:
		a.Logger.InfoContext(r.Context(), "canvas dimensions confirmed",
			slog.String("tenant_id", key.TenantID),
			slog.String("canvas_id", key.CanvasID),
			slog.Int("width", mutation.Dimensions.Width),
			slog.Int("height", mutation.Dimensions.Height),
			slog.Int64("revision", result.Revision),
		)
		writeJSON(w, http.StatusOK, dimensionsResult{
			CanvasID: key.CanvasID,
			Width:    mutation.Dimensions.Width,
			Height:   mutation.Dimensions.Height,
			Revision: result.Revision,
		})
	case errors.Is(err, access.ErrAccessDenied):
		a.responder.Error(w, r, httpx.ErrForbidden, err)
	case errors.Is(err, canvas.ErrNotFound):
		// A missing canvas is the access denial of the read route.
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	case errors.Is(err, canvas.ErrInactive):
		// A deactivation confirmed after the reauthorization (US-003) is the
		// tenant route's access denial.
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	case errors.Is(err, canvas.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	case errors.Is(err, canvas.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// authenticate resolves the session cookie. An invalid session clears the
// cookie and answers unauthorized.
func (a *api) authenticate(w http.ResponseWriter, r *http.Request) (secret.Value, access.Principal, bool) {
	token := sessionToken(r)
	principal, err := a.Accounts.Authenticate(r.Context(), token)
	if errors.Is(err, access.ErrSessionInvalid) {
		if !token.IsZero() {
			clearSessionCookie(w)
		}
		a.responder.Error(w, r, httpx.ErrUnauthorized, err)
		return secret.Value{}, access.Principal{}, false
	}
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return secret.Value{}, access.Principal{}, false
	}
	return token, principal, true
}

func sessionToken(r *http.Request) secret.Value {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return secret.Value{}
	}
	return secret.New(secret.KindSessionToken, cookie.Value)
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// accountKey keys the per-account limit by normalized email, so casing or
// spacing variants share one budget.
func accountKey(email string) string {
	if normalized, ok := access.NormalizeEmail(email); ok {
		return normalized
	}
	key := strings.ToLower(strings.TrimSpace(email))
	if len(key) > access.MaxEmailBytes {
		key = key[:access.MaxEmailBytes]
	}
	return key
}

func decodeJSON(w http.ResponseWriter, r *http.Request, into any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("content type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.New("malformed JSON body")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
