// Package httpx holds HTTP primitives that keep secrets out of responses,
// caches, and Referer headers (TE-003).
package httpx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

// CorrelationHeader carries the server-assigned correlation ID back to the client.
const CorrelationHeader = "X-Correlation-ID"

// revalidatedCacheControl lets the browser keep a private copy that it must
// revalidate before each use, so an ETag can answer 304. Shared caches never
// store it.
const revalidatedCacheControl = "private, no-cache"

func setPrivateHeaders(h http.Header) {
	setCacheHeaders(h, "no-store")
}

func setCacheHeaders(h http.Header, cacheControl string) {
	h.Set("Cache-Control", cacheControl)
	h.Set("Referrer-Policy", "no-referrer")
}

// Revalidate marks the answer about to be written as a private resource
// that the browser may keep and must revalidate: Private then sends
// "private, no-cache" instead of no-store. Call it right before writing a
// successful or not-modified answer; outside Private it sets the header
// itself.
func Revalidate(w http.ResponseWriter) {
	for {
		switch rw := w.(type) {
		case *privateWriter:
			rw.revalidate = true
			setCacheHeaders(rw.Header(), revalidatedCacheControl)
			return
		case interface{ Unwrap() http.ResponseWriter }:
			w = rw.Unwrap()
		default:
			setCacheHeaders(w.Header(), revalidatedCacheControl)
			return
		}
	}
}

// Private marks every response of next as non-cacheable and referrer-free.
// The headers are reapplied when the status is written, so a handler cannot
// weaken them.
func Private(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setPrivateHeaders(w.Header())
		pw := &privateWriter{ResponseWriter: w}
		next.ServeHTTP(pw, r)
		if !pw.wroteHeader {
			setPrivateHeaders(w.Header())
		}
	})
}

type privateWriter struct {
	http.ResponseWriter
	wroteHeader bool
	// revalidate is set by Revalidate.
	revalidate bool
}

func (w *privateWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if w.revalidate {
			setCacheHeaders(w.Header(), revalidatedCacheControl)
		} else {
			setPrivateHeaders(w.Header())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *privateWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// FlushError commits the private headers before flushing, so
// http.ResponseController cannot send a handler's weaker headers first.
func (w *privateWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Flush implements http.Flusher with the same header guarantee.
func (w *privateWriter) Flush() { _ = w.FlushError() }

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *privateWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Correlate assigns a fresh correlation ID to each request. Inbound IDs are
// ignored until their trust rules exist (CTRL-TRACE-01, TE-015).
func Correlate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := correlation.New()
		if err != nil {
			setPrivateHeaders(w.Header())
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		w.Header().Set(CorrelationHeader, id.String())
		next.ServeHTTP(w, r.WithContext(correlation.WithID(r.Context(), id)))
	})
}

// ErrorCode is the closed vocabulary of public error identifiers.
type ErrorCode string

const (
	ErrBadRequest   ErrorCode = "bad_request"
	ErrUnauthorized ErrorCode = "unauthorized"
	ErrForbidden    ErrorCode = "forbidden"
	// ErrSubscriptionInactive tells the owner of a deactivated tenant why
	// its own canvas is denied (US-003); every other denial is ErrForbidden.
	ErrSubscriptionInactive ErrorCode = "subscription_inactive"
	ErrNotFound             ErrorCode = "not_found"
	ErrConflict             ErrorCode = "conflict"
	// ErrIdentityTaken reports a registration for an email that already has
	// an account (US-001); without email verification it cannot be hidden.
	ErrIdentityTaken ErrorCode = "identity_taken"
	ErrRateLimited   ErrorCode = "rate_limited"
	ErrInternal      ErrorCode = "internal"

	// Media upload codes (US-016). ErrLengthRequired answers an upload
	// without an exact Content-Length; ErrQuotaExceeded one that does not
	// fit the canvas quota; ErrMediaRejected one that inspection rejected,
	// with its closed rejection code as the reason; ErrUploadInProgress a
	// retry of an upload that has not finished; ErrReferenceExists a
	// reference ID already in use; ErrUploadFailed a storage or inspection
	// failure; ErrMediaUnavailable a server without media storage.
	ErrLengthRequired   ErrorCode = "length_required"
	ErrQuotaExceeded    ErrorCode = "quota_exceeded"
	ErrMediaRejected    ErrorCode = "media_rejected"
	ErrUploadInProgress ErrorCode = "upload_in_progress"
	ErrReferenceExists  ErrorCode = "reference_exists"
	ErrUploadFailed     ErrorCode = "upload_failed"
	ErrMediaUnavailable ErrorCode = "media_unavailable"

	// Object lock codes (US-020). ErrLockSessionGone answers a lock session
	// that ended, whose locks are already free; the client opens a new one.
	// ErrNotLockHolder answers a release of a lock the session does not
	// hold; ErrLockUnavailable a server whose lock service is not serving.
	ErrLockSessionGone ErrorCode = "lock_session_gone"
	ErrNotLockHolder   ErrorCode = "not_lock_holder"
	ErrLockUnavailable ErrorCode = "lock_unavailable"
)

var errorStatus = map[ErrorCode]int{
	ErrBadRequest:           http.StatusBadRequest,
	ErrUnauthorized:         http.StatusUnauthorized,
	ErrForbidden:            http.StatusForbidden,
	ErrSubscriptionInactive: http.StatusForbidden,
	ErrNotFound:             http.StatusNotFound,
	ErrConflict:             http.StatusConflict,
	ErrIdentityTaken:        http.StatusConflict,
	ErrRateLimited:          http.StatusTooManyRequests,
	ErrInternal:             http.StatusInternalServerError,
	ErrLengthRequired:       http.StatusLengthRequired,
	ErrQuotaExceeded:        http.StatusRequestEntityTooLarge,
	ErrMediaRejected:        http.StatusUnprocessableEntity,
	ErrUploadInProgress:     http.StatusConflict,
	ErrReferenceExists:      http.StatusConflict,
	ErrUploadFailed:         http.StatusBadGateway,
	ErrMediaUnavailable:     http.StatusServiceUnavailable,
	ErrLockSessionGone:      http.StatusGone,
	ErrNotLockHolder:        http.StatusConflict,
	ErrLockUnavailable:      http.StatusServiceUnavailable,
}

// maxReasonBytes bounds the public reason of an error.
const maxReasonBytes = 64

// Responder writes public errors. The internal error is logged through the
// redacting logger and never reaches the response.
type Responder struct {
	Logger *slog.Logger
}

type errorBody struct {
	Error         ErrorCode `json:"error"`
	Reason        string    `json:"reason,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

// Error writes code with its fixed status and logs err with the correlation ID.
// An unknown code is treated as ErrInternal.
func (rs Responder) Error(w http.ResponseWriter, r *http.Request, code ErrorCode, err error) {
	rs.write(w, r, code, "", err)
}

// ErrorWithReason writes code like Error and adds reason, a category of a
// closed public vocabulary chosen by the caller, such as a media rejection
// code. A reason that is not a short lower-case identifier is dropped, so no
// input can be reflected through it.
func (rs Responder) ErrorWithReason(w http.ResponseWriter, r *http.Request, code ErrorCode, reason string, err error) {
	if !publicReason(reason) {
		reason = ""
	}
	rs.write(w, r, code, reason, err)
}

func publicReason(reason string) bool {
	if reason == "" || len(reason) > maxReasonBytes {
		return false
	}
	for i := 0; i < len(reason); i++ {
		if (reason[i] < 'a' || reason[i] > 'z') && reason[i] != '_' {
			return false
		}
	}
	return true
}

func (rs Responder) write(w http.ResponseWriter, r *http.Request, code ErrorCode, reason string, err error) {
	status, ok := errorStatus[code]
	if !ok {
		code, status = ErrInternal, errorStatus[ErrInternal]
	}
	logger := rs.Logger
	if logger == nil {
		logger = slog.Default()
	}
	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	logger.LogAttrs(r.Context(), level, "request failed",
		slog.String("error_code", string(code)),
		slog.Int("status", status),
		slog.Any("error", err),
	)

	id, _ := correlation.FromContext(r.Context())
	h := w.Header()
	setPrivateHeaders(h)
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Reason: reason, CorrelationID: id.String()})
}

// Recover turns a handler panic into a generic internal error. The panic value
// is only logged, through the redacting logger.
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	responder := Responder{Logger: logger}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			responder.Error(w, r, ErrInternal, fmt.Errorf("panic: %v", v))
		}()
		next.ServeHTTP(w, r)
	})
}
