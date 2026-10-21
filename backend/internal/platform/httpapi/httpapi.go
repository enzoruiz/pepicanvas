// Package httpapi serves the account, session, and canvas-request HTTP API
// (US-001) and the manual streamer activation and own-canvas read (US-002).
// Every response is private and correlated; unsafe methods must
// come from the configured origin, and authenticated mutations must carry the
// session's CSRF token (CTRL-WEB-01 early primitive; TE-014 owns the control).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

const (
	// SessionCookie holds the session token. The __Host- prefix makes the
	// browser require Secure, Path=/, and no Domain.
	SessionCookie = "__Host-pcs"
	// CSRFHeader carries the session's CSRF token on authenticated mutations.
	CSRFHeader = "X-CSRF-Token"

	maxBodyBytes = 4096
	// maxLimiterKeys bounds each limiter's memory; a full table fails closed.
	maxLimiterKeys = 100_000
)

// Rate is a number of attempts allowed per window.
type Rate struct {
	Limit  int
	Window time.Duration
}

// Limits are the abuse limits of the sign-in and registration routes
// (CTRL-RATE-01 early primitive). Addresses come from the TCP peer.
type Limits struct {
	SignInPerAccount Rate
	SignInPerIP      Rate
	RegisterPerIP    Rate
}

// DefaultLimits are the pilot limits.
var DefaultLimits = Limits{
	SignInPerAccount: Rate{Limit: 10, Window: 15 * time.Minute},
	SignInPerIP:      Rate{Limit: 50, Window: 15 * time.Minute},
	RegisterPerIP:    Rate{Limit: 10, Window: time.Hour},
}

// Accounts is the account and session service.
type Accounts interface {
	Register(ctx context.Context, request access.RegisterRequest) (access.AccountID, error)
	SignIn(ctx context.Context, email string, password secret.Value) (access.Session, error)
	Authenticate(ctx context.Context, token secret.Value) (access.Principal, error)
	SignOut(ctx context.Context, token secret.Value) error
}

// CanvasLoader reads committed canvases.
type CanvasLoader interface {
	Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error)
}

// Subscriptions is the streamer activation service.
type Subscriptions interface {
	Activate(ctx context.Context, request subscription.ActivateRequest) (subscription.Activation, error)
	CanvasOf(ctx context.Context, account access.AccountID) (canvas.Key, error)
	IsOperator(ctx context.Context, account access.AccountID) (bool, error)
}

// Config wires the API.
type Config struct {
	Accounts      Accounts
	Memberships   access.MembershipStore
	Canvases      CanvasLoader
	Subscriptions Subscriptions
	// AllowedOrigin is the exact scheme://host[:port] of the editor.
	AllowedOrigin string
	Logger        *slog.Logger
	// Now is the limiter clock; nil uses time.Now.
	Now    func() time.Time
	Limits Limits
}

type api struct {
	Config
	responder        httpx.Responder
	signInPerAccount *ratelimit.Limiter
	signInPerIP      *ratelimit.Limiter
	registerPerIP    *ratelimit.Limiter
}

// New validates the configuration and returns the API handler.
func New(config Config) (http.Handler, error) {
	if err := validateOrigin(config.AllowedOrigin); err != nil {
		return nil, err
	}
	if config.Accounts == nil || config.Memberships == nil || config.Canvases == nil || config.Subscriptions == nil {
		return nil, errors.New("httpapi: accounts, memberships, canvases, and subscriptions are required")
	}
	for _, rate := range []Rate{config.Limits.SignInPerAccount, config.Limits.SignInPerIP, config.Limits.RegisterPerIP} {
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/accounts", a.register)
	mux.HandleFunc("POST /api/sessions", a.signIn)
	mux.HandleFunc("GET /api/session", a.currentSession)
	mux.HandleFunc("DELETE /api/session", a.signOut)
	mux.HandleFunc("GET /api/tenants/{tenantID}/canvases/{canvasID}", a.canvas)
	mux.HandleFunc("POST /api/activations", a.activate)
	mux.HandleFunc("GET /api/canvas", a.ownCanvas)
	return httpx.Recover(config.Logger, httpx.Correlate(httpx.Private(a.checkOrigin(mux)))), nil
}

func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || origin != u.Scheme+"://"+u.Host {
		return fmt.Errorf("httpapi: allowed origin must be exactly scheme://host[:port]")
	}
	return nil
}

// checkOrigin rejects unsafe methods from any origin but the configured one,
// before routing, body parsing, or the domain run.
func (a *api) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get("Origin") != a.AllowedOrigin {
				a.responder.Error(w, r, httpx.ErrForbidden, errors.New("origin rejected"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type registerBody struct {
	OperationID string `json:"operation_id"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

func (a *api) register(w http.ResponseWriter, r *http.Request) {
	if !a.allow(w, r, a.registerPerIP, clientIP(r), "registration limit reached for address") {
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
	if !a.allow(w, r, a.signInPerIP, clientIP(r), "sign-in limit reached for address") ||
		!a.allow(w, r, a.signInPerAccount, accountKey(body.Email), "sign-in limit reached for account") {
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

// activate lets an operator activate the account of an email. The session
// and its CSRF token are checked before the body is read; the operator role
// is checked by the domain before the account is looked up.
func (a *api) activate(w http.ResponseWriter, r *http.Request) {
	token, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	if !access.VerifyCSRF(token, r.Header.Get(CSRFHeader)) {
		a.responder.Error(w, r, httpx.ErrForbidden, errors.New("csrf token rejected"))
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
	if !access.VerifyCSRF(token, r.Header.Get(CSRFHeader)) {
		a.responder.Error(w, r, httpx.ErrForbidden, errors.New("csrf token rejected"))
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

// ownCanvas answers the canvas of the tenant the account owns. Every denial,
// including a missing canvas, is the forbidden answer of the canvas route.
func (a *api) ownCanvas(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, err := a.Subscriptions.CanvasOf(r.Context(), principal.AccountID)
	if errors.Is(err, access.ErrAccessDenied) {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
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
	key := canvas.Key{TenantID: r.PathValue("tenantID"), CanvasID: r.PathValue("canvasID")}
	if err := canvas.ValidateKey(key); err != nil {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
		return
	}
	membership, found, err := a.Memberships.MembershipOf(r.Context(), principal.AccountID, key.TenantID)
	if err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	if err := access.Check(access.CanvasRequest(membership, found, key.TenantID, access.ActionView)); err != nil {
		a.responder.Error(w, r, httpx.ErrForbidden, err)
		return
	}
	loaded, ok := a.load(w, r, key)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newCanvasBody(loaded))
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

func (a *api) allow(w http.ResponseWriter, r *http.Request, limiter *ratelimit.Limiter, key, reason string) bool {
	ok, retry := limiter.Allow(key)
	if ok {
		return true
	}
	seconds := int((retry + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(max(seconds, 1)))
	a.responder.Error(w, r, httpx.ErrRateLimited, errors.New(reason))
	return false
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

// clientIP is the TCP peer address. Trusting forwarding headers is TE-014's decision.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
