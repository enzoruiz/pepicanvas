// Package ingress guards every HTTP and WebSocket entry point before routing
// reaches the domain (TE-014). It owns CTRL-WEB-01 (exact Origin on unsafe
// requests and before every upgrade, session-bound CSRF, closed CORS, security
// headers) and CTRL-RATE-01 (deterministic in-memory limits per IP, session,
// and account, answered with 429 and counted over closed enums).
//
// Client addresses are the TCP peer; forwarding headers are ignored until a
// trusted proxy is an explicit decision.
package ingress

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	// SessionCookie holds the session token (the httpapi cookie).
	SessionCookie = "__Host-pcs"
	// CSRFHeader carries the CSRF token bound to the session.
	CSRFHeader = "X-CSRF-Token"

	// maxLimiterKeys bounds each limiter's memory; a full table fails closed.
	maxLimiterKeys = 100_000
	// unmatched is the route label of a request no registered pattern serves.
	unmatched = "unmatched"
)

// Class declares what an unsafe route needs before its handler runs. An unsafe
// request to a route registered without a class is rejected.
type Class int

const (
	// Anonymous routes need an exact Origin only.
	Anonymous Class = iota + 1
	// Session routes also need the CSRF token of the session cookie, when one
	// is present, and spend the per-session mutation budget.
	Session
)

// Surface is the closed set of ingress surfaces in metrics and logs.
type Surface string

const (
	SurfaceHTTP      Surface = "http"
	SurfaceWebSocket Surface = "websocket"
)

// Reason is the closed set of rejection reasons in metrics and logs.
type Reason string

const (
	ReasonOrigin       Reason = "origin"
	ReasonCSRF         Reason = "csrf"
	ReasonPreflight    Reason = "cors_preflight"
	ReasonUnclassified Reason = "unclassified"
	ReasonNotWebSocket Reason = "not_websocket"
	ReasonRateIP       Reason = "rate_ip"
	ReasonRateSession  Reason = "rate_session"
	ReasonRateAccount  Reason = "rate_account"
	ReasonLimiterFull  Reason = "limiter_full"
)

const (
	numSurfaces = 2
	numReasons  = 9
)

var (
	surfaces = [numSurfaces]Surface{SurfaceHTTP, SurfaceWebSocket}
	reasons  = [numReasons]Reason{
		ReasonOrigin, ReasonCSRF, ReasonPreflight, ReasonUnclassified, ReasonNotWebSocket,
		ReasonRateIP, ReasonRateSession, ReasonRateAccount, ReasonLimiterFull,
	}
)

// Surfaces returns every surface in snapshot order.
func Surfaces() []Surface { return append([]Surface(nil), surfaces[:]...) }

// Reasons returns every reason in snapshot order.
func Reasons() []Reason { return append([]Reason(nil), reasons[:]...) }

// Count is one rejection counter.
type Count struct {
	Surface Surface
	Reason  Reason
	Value   uint64
}

// Limits are the ingress abuse limits.
type Limits struct {
	UnsafePerIP         ratelimit.Rate
	MutationsPerSession ratelimit.Rate
	UpgradesPerIP       ratelimit.Rate
	UpgradesPerSession  ratelimit.Rate
}

// DefaultLimits are the pilot limits.
var DefaultLimits = Limits{
	UnsafePerIP:         ratelimit.Rate{Limit: 300, Window: time.Minute},
	MutationsPerSession: ratelimit.Rate{Limit: 120, Window: time.Minute},
	UpgradesPerIP:       ratelimit.Rate{Limit: 30, Window: time.Minute},
	UpgradesPerSession:  ratelimit.Rate{Limit: 10, Window: time.Minute},
}

// Config configures the ingress.
type Config struct {
	// AllowedOrigins are the exact scheme://host[:port] origins of the app.
	AllowedOrigins []string
	Limits         Limits
	Logger         *slog.Logger
	// Now is the limiter time source; nil uses time.Now.
	Now func() time.Time
}

// Ingress holds the origin set, the limiters, and the rejection counters.
type Ingress struct {
	origins             map[string]bool
	hsts                bool
	responder           httpx.Responder
	unsafePerIP         *ratelimit.Limiter
	mutationsPerSession *ratelimit.Limiter
	upgradesPerIP       *ratelimit.Limiter
	upgradesPerSession  *ratelimit.Limiter
	counts              [numSurfaces][numReasons]atomic.Uint64
}

// New validates config. A configuration that would weaken a control, such as
// a wildcard origin or a non-positive limit, is an error, so the process
// refuses to start instead of serving with the control off.
func New(config Config) (*Ingress, error) {
	if len(config.AllowedOrigins) == 0 {
		return nil, errors.New("ingress: at least one allowed origin is required")
	}
	in := &Ingress{origins: map[string]bool{}, hsts: true}
	for _, origin := range config.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return nil, err
		}
		in.origins[origin] = true
		in.hsts = in.hsts && strings.HasPrefix(origin, "https://")
	}
	l := config.Limits
	for _, rate := range []ratelimit.Rate{l.UnsafePerIP, l.MutationsPerSession, l.UpgradesPerIP, l.UpgradesPerSession} {
		if rate.Limit < 1 || rate.Window <= 0 {
			return nil, errors.New("ingress: every limit needs a positive count and window")
		}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	limiter := func(rate ratelimit.Rate) *ratelimit.Limiter {
		return ratelimit.New(rate.Limit, rate.Window, maxLimiterKeys, config.Now)
	}
	in.responder = httpx.Responder{Logger: config.Logger}
	in.unsafePerIP = limiter(l.UnsafePerIP)
	in.mutationsPerSession = limiter(l.MutationsPerSession)
	in.upgradesPerIP = limiter(l.UpgradesPerIP)
	in.upgradesPerSession = limiter(l.UpgradesPerSession)
	return in, nil
}

func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || origin != u.Scheme+"://"+u.Host ||
		strings.Contains(u.Host, "*") || !canonicalHost(u) {
		return fmt.Errorf("ingress: allowed origin must be exactly scheme://host[:port]")
	}
	return nil
}

// canonicalHost reports whether u's host is spelled the way a browser sends
// it in Origin: lower-case ASCII, without an empty or default port. Any other
// spelling could never match and would only hide a configuration typo.
func canonicalHost(u *url.URL) bool {
	if u.Host != strings.ToLower(u.Host) || strings.HasSuffix(u.Host, ":") {
		return false
	}
	for _, c := range u.Host {
		if c > unicode.MaxASCII {
			return false
		}
	}
	port := u.Port()
	return !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80")
}

// Protect guards mux: every answer carries the security headers, every
// preflight is refused, and every unsafe request needs an exact Origin, a
// route registered through Route, and the per-IP budget before mux serves it.
// Unmatched requests keep the mux 404 and 405 answers.
func (in *Ingress) Protect(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.setAPIHeaders(w.Header())
		h, route := match(mux, r)
		if r.Method == http.MethodOptions {
			in.reject(w, r, SurfaceHTTP, ReasonPreflight, route)
			return
		}
		if !safe(r.Method) {
			if !in.originAllowed(r) {
				in.reject(w, r, SurfaceHTTP, ReasonOrigin, route)
				return
			}
			if rt, ok := h.(*classified); route != unmatched && (!ok || rt.in != in) {
				in.reject(w, r, SurfaceHTTP, ReasonUnclassified, route)
				return
			}
			if !in.allow(w, r, in.unsafePerIP, ClientIP(r), SurfaceHTTP, ReasonRateIP, route) {
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// redirectType is the mux's redirect handler, whose pattern is a request path.
var redirectType = reflect.TypeOf(http.RedirectHandler("/", http.StatusMovedPermanently))

// match returns the handler mux would run and its pattern, or unmatched when
// no pattern serves r, so only registered patterns ever reach logs.
func match(mux *http.ServeMux, r *http.Request) (http.Handler, string) {
	h, pattern := mux.Handler(r)
	if pattern == "" || reflect.TypeOf(h) == redirectType {
		return h, unmatched
	}
	return h, pattern
}

// classified marks a handler registered through Route.
type classified struct {
	in    *Ingress
	class Class
	next  http.Handler
}

// Route registers h under class. Protect rejects unsafe requests to any other
// handler.
func (in *Ingress) Route(class Class, h http.Handler) http.Handler {
	return &classified{in: in, class: class, next: h}
}

func (c *classified) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in, route := c.in, routeOf(r)
	if !safe(r.Method) {
		// Protect already checked the Origin; repeat it so a classified route
		// mounted without Protect still fails closed.
		if !in.originAllowed(r) {
			in.reject(w, r, SurfaceHTTP, ReasonOrigin, route)
			return
		}
		if cookie, err := r.Cookie(SessionCookie); c.class == Session && err == nil {
			token := secret.New(secret.KindSessionToken, cookie.Value)
			if !access.VerifyCSRF(token, r.Header.Get(CSRFHeader)) {
				in.reject(w, r, SurfaceHTTP, ReasonCSRF, route)
				return
			}
			if !in.allow(w, r, in.mutationsPerSession, SessionKey(cookie.Value), SurfaceHTTP, ReasonRateSession, route) {
				return
			}
		}
	}
	c.next.ServeHTTP(w, r)
}

// Upgrade guards a WebSocket upgrade route: exact Origin, handshake shape,
// then the per-session and per-IP upgrade budgets, before h can upgrade.
func (in *Ingress) Upgrade(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routeOf(r)
		if !in.originAllowed(r) {
			in.reject(w, r, SurfaceWebSocket, ReasonOrigin, route)
			return
		}
		if r.Method != http.MethodGet || !hasToken(r.Header, "Connection", "upgrade") || !hasToken(r.Header, "Upgrade", "websocket") {
			in.reject(w, r, SurfaceWebSocket, ReasonNotWebSocket, route)
			return
		}
		// The session budget is only peeked before the address budget, so a
		// session-limited attempt spends neither, and an attempt the address
		// budget refuses never takes a slot in the shared session table.
		sessionKey := ""
		if cookie, err := r.Cookie(SessionCookie); err == nil {
			sessionKey = SessionKey(cookie.Value)
			if !in.admit(w, r, in.upgradesPerSession.Peek(sessionKey), SurfaceWebSocket, ReasonRateSession, route) {
				return
			}
		}
		if !in.allow(w, r, in.upgradesPerIP, ClientIP(r), SurfaceWebSocket, ReasonRateIP, route) {
			return
		}
		if sessionKey != "" && !in.allow(w, r, in.upgradesPerSession, sessionKey, SurfaceWebSocket, ReasonRateSession, route) {
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Static serves h for GET and HEAD with frame and sniffing protection and
// answers every other method with 405.
func (in *Ingress) Static(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		in.setHSTS(h)
		if !safe(r.Method) {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Allow spends one attempt of key in limiter. When it is denied, Allow answers
// 429 with Retry-After, records reason (or ReasonLimiterFull when the key
// table is full), and returns false. Handlers use it for their own limits.
func (in *Ingress) Allow(w http.ResponseWriter, r *http.Request, limiter *ratelimit.Limiter, key string, reason Reason) bool {
	return in.allow(w, r, limiter, key, SurfaceHTTP, reason, routeOf(r))
}

func (in *Ingress) allow(w http.ResponseWriter, r *http.Request, limiter *ratelimit.Limiter, key string, surface Surface, reason Reason, route string) bool {
	return in.admit(w, r, limiter.Check(key), surface, reason, route)
}

// admit answers 429 for a denied limiter result and reports whether the
// request may continue.
func (in *Ingress) admit(w http.ResponseWriter, r *http.Request, res ratelimit.Result, surface Surface, reason Reason, route string) bool {
	if res.Allowed {
		return true
	}
	if res.Full {
		reason = ReasonLimiterFull
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds(res.RetryAfter), 10))
	in.respond(w, r, httpx.ErrRateLimited, surface, reason, route)
	return false
}

// retrySeconds rounds d up to whole seconds, at least one.
func retrySeconds(d time.Duration) int64 {
	return max(1, int64((d+time.Second-1)/time.Second))
}

// Snapshot returns every counter in Surfaces then Reasons order.
func (in *Ingress) Snapshot() []Count {
	out := make([]Count, 0, len(surfaces)*len(reasons))
	for i, s := range surfaces {
		for j, reason := range reasons {
			out = append(out, Count{Surface: s, Reason: reason, Value: in.counts[i][j].Load()})
		}
	}
	return out
}

// SessionKey is the limiter key of a session: the hex SHA-256 of its token, so
// limiter tables never hold tokens.
func SessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ClientIP is the TCP peer address of r. Forwarding headers are ignored.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rejection is the logged error of a refused request. It carries only closed
// enums and a registered pattern, never origins, paths, cookies, or tokens.
type rejection struct {
	surface Surface
	reason  Reason
	route   string
}

func (e rejection) Error() string {
	return fmt.Sprintf("ingress rejected: surface=%s reason=%s route=%s", e.surface, e.reason, e.route)
}

var rejectionCode = map[Reason]httpx.ErrorCode{
	ReasonOrigin:       httpx.ErrForbidden,
	ReasonCSRF:         httpx.ErrForbidden,
	ReasonPreflight:    httpx.ErrForbidden,
	ReasonUnclassified: httpx.ErrForbidden,
	ReasonNotWebSocket: httpx.ErrBadRequest,
}

func (in *Ingress) reject(w http.ResponseWriter, r *http.Request, surface Surface, reason Reason, route string) {
	in.respond(w, r, rejectionCode[reason], surface, reason, route)
}

func (in *Ingress) respond(w http.ResponseWriter, r *http.Request, code httpx.ErrorCode, surface Surface, reason Reason, route string) {
	in.record(surface, reason)
	in.responder.Error(w, r, code, rejection{surface: surface, reason: reason, route: route})
}

// record counts a rejection. Reasons outside the closed set are not counted,
// so the series stay bounded.
func (in *Ingress) record(surface Surface, reason Reason) {
	i, j := index(surfaces[:], surface), index(reasons[:], reason)
	if i >= 0 && j >= 0 {
		in.counts[i][j].Add(1)
	}
}

func index[T comparable](set []T, v T) int {
	for i, s := range set {
		if s == v {
			return i
		}
	}
	return -1
}

func (in *Ingress) setAPIHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	in.setHSTS(h)
}

// setHSTS pins HTTPS only when every allowed origin is HTTPS, so a local
// plain-HTTP deployment is not locked out.
func (in *Ingress) setHSTS(h http.Header) {
	if in.hsts {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (in *Ingress) originAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	return len(values) == 1 && in.origins[values[0]]
}

// safe reports the methods without side effects; every other method is unsafe.
func safe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return unmatched
	}
	return r.Pattern
}

// hasToken reports whether the comma-separated header name lists token,
// compared case-insensitively.
func hasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
