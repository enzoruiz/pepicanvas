package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
)

// uploadContentType is the only accepted upload body type: the raw bytes.
const uploadContentType = "application/octet-stream"

var errMediaUnavailable = errors.New("media storage is not configured")

type usageBody struct {
	QuotaBytes     int64 `json:"quota_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	ReservedBytes  int64 `json:"reserved_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

// newUsageBody is the aggregate accounting of a canvas. It never names an
// asset or the size of one.
func newUsageBody(usage media.Usage) usageBody {
	return usageBody{
		QuotaBytes:     usage.QuotaBytes,
		UsedBytes:      usage.UsedBytes,
		ReservedBytes:  usage.ReservedBytes,
		AvailableBytes: usage.Available(),
	}
}

type uploadResult struct {
	AssetID string    `json:"asset_id"`
	Kind    string    `json:"kind"`
	Usage   usageBody `json:"usage"`
}

type attachBody struct {
	OperationID string `json:"operation_id"`
	AssetID     string `json:"asset_id"`
	ReferenceID string `json:"reference_id"`
}

type attachResult struct {
	AssetID string    `json:"asset_id"`
	Usage   usageBody `json:"usage"`
}

type detachBody struct {
	OperationID string `json:"operation_id"`
}

type detachResult struct {
	Usage usageBody `json:"usage"`
}

// guardedMedia reauthorizes the caller's edit access immediately before each
// media command commits, so access lost after the route check is denied at
// the commit (US-016 S9). Abandon is never guarded: a failed upload always
// releases its reservation.
type guardedMedia struct {
	media.Store
	memberships access.MembershipStore
	account     access.AccountID
}

func (g guardedMedia) Apply(ctx context.Context, key media.CanvasKey, operation media.Operation, command media.Command) (media.Result, error) {
	if _, abandon := command.(media.Abandon); abandon {
		return g.Store.Apply(ctx, key, operation, command)
	}
	var (
		result  media.Result
		loadErr error
	)
	err := access.GuardMutation(func() (access.Request, error) {
		membership, found, err := g.memberships.MembershipOf(ctx, g.account, key.TenantID)
		loadErr = err
		return access.CanvasRequest(membership, found, key.TenantID, access.ActionEdit), err
	}, func() error {
		var err error
		result, err = g.Store.Apply(ctx, key, operation, command)
		return err
	})
	if loadErr != nil {
		return media.Result{}, fmt.Errorf("reauthorize media command: %w", loadErr)
	}
	return result, err
}

func (a *api) guarded(principal access.Principal) guardedMedia {
	return guardedMedia{Store: a.Media, memberships: a.Memberships, account: principal.AccountID}
}

func mediaKey(key canvas.Key) media.CanvasKey {
	return media.CanvasKey{TenantID: key.TenantID, CanvasID: key.CanvasID}
}

func correlationOf(r *http.Request) string {
	id, _ := correlation.FromContext(r.Context())
	return id.String()
}

// mediaUsage answers the aggregate storage accounting of a canvas the caller
// may view. Every denial, including a missing canvas, is the forbidden answer
// of the canvas route.
func (a *api) mediaUsage(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionView)
	if !ok {
		return
	}
	if a.Media == nil {
		a.responder.Error(w, r, httpx.ErrMediaUnavailable, errMediaUnavailable)
		return
	}
	usage, err := a.Media.Usage(r.Context(), mediaKey(key))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, newUsageBody(usage))
	case errors.Is(err, media.ErrNotFound), errors.Is(err, media.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// uploadMedia stores the raw request body as a new asset of the canvas under
// reference_id, or with replace=true as the replacement of the asset that
// reference names. The ingress checks Origin and the CSRF token before the
// session is read; the edit access and then the per-account upload limit are
// checked before the request is parsed, and the declared size, the exact
// Content-Length, before anything is reserved. The caller is reauthorized
// before each commit. A retry under the same operation ID repeats the answer
// without a second effect.
func (a *api) uploadMedia(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionEdit)
	if !ok {
		return
	}
	if a.Media == nil || a.Storage == nil || a.Inspector == nil {
		a.responder.Error(w, r, httpx.ErrMediaUnavailable, errMediaUnavailable)
		return
	}
	if !a.Ingress.Allow(w, r, a.uploadsPerAccount, string(principal.AccountID), ingress.ReasonRateAccount) {
		return
	}
	query := r.URL.Query()
	var replace bool
	switch query.Get("replace") {
	case "", "false":
	case "true":
		replace = true
	default:
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("replace must be true or false"))
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != uploadContentType {
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("content type must be "+uploadContentType))
		return
	}
	switch {
	case r.ContentLength < 0:
		a.responder.Error(w, r, httpx.ErrLengthRequired, errors.New("upload needs an exact Content-Length"))
		return
	case r.ContentLength == 0:
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("upload body is empty"))
		return
	case r.ContentLength > media.QuotaBytes:
		// It could never fit, so nothing is reserved.
		a.responder.Error(w, r, httpx.ErrQuotaExceeded, media.ErrQuotaExceeded)
		return
	}
	uploads := media.NewUploads(a.guarded(principal), a.Storage, a.Inspector)
	result, err := uploads.Upload(r.Context(), media.UploadRequest{
		Key:           mediaKey(key),
		Operation:     media.Operation{ID: query.Get("operation_id"), CorrelationID: correlationOf(r)},
		DeclaredBytes: r.ContentLength,
		ReferenceID:   query.Get("reference_id"),
		Replace:       replace,
		Body:          r.Body,
	})
	if err != nil {
		a.mediaError(w, r, key, err)
		return
	}
	a.logUsage(r, "media upload confirmed", key, result.Usage, slog.String("asset_id", result.AssetID), slog.String("kind", string(result.Kind)))
	writeJSON(w, http.StatusCreated, uploadResult{AssetID: result.AssetID, Kind: string(result.Kind), Usage: newUsageBody(result.Usage)})
}

// attachMedia adds a reference to a ready asset of the canvas. It runs in the
// order of uploadMedia, without the upload limit.
func (a *api) attachMedia(w http.ResponseWriter, r *http.Request) {
	principal, key, ok := a.authorizeMediaMutation(w, r)
	if !ok {
		return
	}
	var body attachBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	result, err := a.guarded(principal).Apply(r.Context(), mediaKey(key),
		media.Operation{ID: body.OperationID, CorrelationID: correlationOf(r)},
		media.Attach{AssetID: body.AssetID, ReferenceID: body.ReferenceID})
	if err != nil {
		a.mediaError(w, r, key, err)
		return
	}
	a.logUsage(r, "media reference attached", key, result.Usage, slog.String("asset_id", result.AssetID))
	writeJSON(w, http.StatusOK, attachResult{AssetID: result.AssetID, Usage: newUsageBody(result.Usage)})
}

// detachMedia removes a reference of the canvas. Removing the last reference
// of an asset releases its bytes from the usage at once.
func (a *api) detachMedia(w http.ResponseWriter, r *http.Request) {
	principal, key, ok := a.authorizeMediaMutation(w, r)
	if !ok {
		return
	}
	var body detachBody
	if err := decodeJSON(w, r, &body); err != nil {
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
		return
	}
	result, err := a.guarded(principal).Apply(r.Context(), mediaKey(key),
		media.Operation{ID: body.OperationID, CorrelationID: correlationOf(r)},
		media.Detach{ReferenceID: r.PathValue("referenceID")})
	if err != nil {
		a.mediaError(w, r, key, err)
		return
	}
	a.logUsage(r, "media reference detached", key, result.Usage)
	writeJSON(w, http.StatusOK, detachResult{Usage: newUsageBody(result.Usage)})
}

// authorizeMediaMutation checks the session, the edit access to the canvas of
// the path, and that media is configured, before the body is read.
func (a *api) authorizeMediaMutation(w http.ResponseWriter, r *http.Request) (access.Principal, canvas.Key, bool) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return access.Principal{}, canvas.Key{}, false
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionEdit)
	if !ok {
		return access.Principal{}, canvas.Key{}, false
	}
	if a.Media == nil {
		a.responder.Error(w, r, httpx.ErrMediaUnavailable, errMediaUnavailable)
		return access.Principal{}, canvas.Key{}, false
	}
	return principal, key, true
}

// logUsage logs a confirmed media mutation with the canvas and its aggregate
// usage. Reference IDs, storage keys, and bytes are never logged.
func (a *api) logUsage(r *http.Request, msg string, key canvas.Key, usage media.Usage, attrs ...slog.Attr) {
	attrs = append([]slog.Attr{
		slog.String("tenant_id", key.TenantID),
		slog.String("canvas_id", key.CanvasID),
		slog.Int64("quota_bytes", usage.QuotaBytes),
		slog.Int64("used_bytes", usage.UsedBytes),
		slog.Int64("reserved_bytes", usage.ReservedBytes),
	}, attrs...)
	a.Logger.LogAttrs(r.Context(), slog.LevelInfo, msg, attrs...)
}

// mediaError maps a media error to its public answer. Internal errors are
// only logged.
func (a *api) mediaError(w http.ResponseWriter, r *http.Request, key canvas.Key, err error) {
	if rejection, ok := errors.AsType[*media.Rejection](err); ok {
		a.responder.ErrorWithReason(w, r, httpx.ErrMediaRejected, string(rejection.Code()), err)
		return
	}
	switch {
	case errors.Is(err, access.ErrAccessDenied), errors.Is(err, media.ErrInactive):
		// A deactivation confirmed after the reauthorization (US-003) is the
		// tenant route's access denial.
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	case errors.Is(err, media.ErrQuotaExceeded):
		a.responder.Error(w, r, httpx.ErrQuotaExceeded, err)
	case errors.Is(err, media.ErrUploadInProgress):
		a.responder.Error(w, r, httpx.ErrUploadInProgress, err)
	case errors.Is(err, media.ErrOperationConflict):
		a.responder.Error(w, r, httpx.ErrConflict, err)
	case errors.Is(err, media.ErrReferenceExists):
		a.responder.Error(w, r, httpx.ErrReferenceExists, err)
	case errors.Is(err, media.ErrNotFound):
		a.mediaNotFound(w, r, key, err)
	case errors.Is(err, media.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrBadRequest, err)
	case errors.Is(err, media.ErrUploadFailed), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// A cancelled upload is answered like its retry, which repeats the
		// recorded abandon.
		a.responder.Error(w, r, httpx.ErrUploadFailed, err)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
	}
}

// mediaNotFound answers a missing asset or reference of a canvas the caller
// may edit as not found, and a missing canvas as the canvas route's access
// denial.
func (a *api) mediaNotFound(w http.ResponseWriter, r *http.Request, key canvas.Key, err error) {
	_, loadErr := a.Canvases.Load(r.Context(), key)
	switch {
	case loadErr == nil:
		a.responder.Error(w, r, httpx.ErrNotFound, err)
	case errors.Is(loadErr, canvas.ErrNotFound), errors.Is(loadErr, canvas.ErrInvalid):
		a.responder.Error(w, r, httpx.ErrForbidden, access.ErrAccessDenied)
	default:
		a.responder.Error(w, r, httpx.ErrInternal, loadErr)
	}
}
