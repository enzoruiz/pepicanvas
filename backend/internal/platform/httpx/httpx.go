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

func setPrivateHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
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
}

func (w *privateWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		setPrivateHeaders(w.Header())
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
	ErrNotFound     ErrorCode = "not_found"
	ErrConflict     ErrorCode = "conflict"
	// ErrIdentityTaken reports a registration for an email that already has
	// an account (US-001); without email verification it cannot be hidden.
	ErrIdentityTaken ErrorCode = "identity_taken"
	ErrRateLimited   ErrorCode = "rate_limited"
	ErrInternal      ErrorCode = "internal"
)

var errorStatus = map[ErrorCode]int{
	ErrBadRequest:    http.StatusBadRequest,
	ErrUnauthorized:  http.StatusUnauthorized,
	ErrForbidden:     http.StatusForbidden,
	ErrNotFound:      http.StatusNotFound,
	ErrConflict:      http.StatusConflict,
	ErrIdentityTaken: http.StatusConflict,
	ErrRateLimited:   http.StatusTooManyRequests,
	ErrInternal:      http.StatusInternalServerError,
}

// Responder writes public errors. The internal error is logged through the
// redacting logger and never reaches the response.
type Responder struct {
	Logger *slog.Logger
}

type errorBody struct {
	Error         ErrorCode `json:"error"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

// Error writes code with its fixed status and logs err with the correlation ID.
// An unknown code is treated as ErrInternal.
func (rs Responder) Error(w http.ResponseWriter, r *http.Request, code ErrorCode, err error) {
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
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, CorrelationID: id.String()})
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
