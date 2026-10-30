package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
)

// uploadContentType is the upload body type that declares no media type.
const uploadContentType = "application/octet-stream"

// imageTypes are the image media types an upload may declare (US-011), which
// the inspection must then find, and the only types the content route serves.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// sniffBytes is how much of a stored file http.DetectContentType reads.
const sniffBytes = 512

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

// uploadResult is the upload answer. ObjectID names the canvas image object
// the upload created, equal to AssetID; it is omitted when the upload created
// none (a replacement, audio, or video).
type uploadResult struct {
	AssetID  string    `json:"asset_id"`
	ObjectID string    `json:"object_id,omitempty"`
	Kind     string    `json:"kind"`
	Usage    usageBody `json:"usage"`
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
// the commit (US-016 S9), and names the caller as the command's actor, so the
// store rechecks the membership inside the commit and a revocation confirmed
// after the reauthorization still rejects it (US-006 S2). Abandon is never
// guarded: a failed upload always releases its reservation.
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
		operation.AccountID = string(g.account)
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
// of the canvas route. The caller is reauthorized after the read and before
// the answer is written.
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
		a.deliver(w, r, principal, key, newUsageBody(usage))
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
// checked before the request is parsed, and the content type and the
// declared size, the exact Content-Length, before anything is reserved. The
// body is application/octet-stream, which declares nothing, or one of the
// image types, which the inspection must confirm; another image type, SVG
// included, is rejected as an unsupported kind, and a declared image above
// the image byte limit as too large. The caller is reauthorized before each
// commit. A retry under the same operation ID repeats the answer without a
// second effect.
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
	var declared string
	switch mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); {
	case err == nil && mediaType == uploadContentType:
	case err == nil && imageTypes[mediaType]:
		declared = mediaType
	case err == nil && strings.HasPrefix(mediaType, "image/"):
		a.mediaError(w, r, key, media.Reject(media.RejectionUnsupportedKind))
		return
	default:
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("content type must be "+uploadContentType+" or a supported image type"))
		return
	}
	switch {
	case r.ContentLength < 0:
		a.responder.Error(w, r, httpx.ErrLengthRequired, errors.New("upload needs an exact Content-Length"))
		return
	case r.ContentLength == 0:
		a.responder.Error(w, r, httpx.ErrBadRequest, errors.New("upload body is empty"))
		return
	case declared != "" && r.ContentLength > media.DefaultLimits().MaxImageBytes:
		// The inspection would reject it, so nothing is reserved or read.
		a.mediaError(w, r, key, media.Reject(media.RejectionInputTooLarge))
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
		DeclaredType:  declared,
		Body:          r.Body,
	})
	if err != nil {
		a.mediaError(w, r, key, err)
		return
	}
	a.logUsage(r, "media upload confirmed", key, result.Usage, slog.String("asset_id", result.AssetID), slog.String("kind", string(result.Kind)))
	writeJSON(w, http.StatusCreated, uploadResult{AssetID: result.AssetID, ObjectID: result.ObjectID, Kind: string(result.Kind), Usage: newUsageBody(result.Usage)})
}

// mediaContent serves the bytes of the ready image or GIF that a reference of
// the canvas names, to a caller who may view the canvas; every denial is the
// canvas route's forbidden. A missing reference, one of another canvas, an
// asset that is not ready, and audio or video (until US-012) are the same
// not found. The type is sniffed from the stored bytes and must be one of
// the image types, matching the asset's kind; anything else is an internal
// error, never served under a guessed type. The answer is private and
// revalidated against the asset ID as its ETag. The caller is reauthorized
// after the read and before the answer, revalidation included, so bytes read
// before a revocation are never served after it (US-006 S8). Neither the URL
// nor the logs hold a storage key.
func (a *api) mediaContent(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := a.authorizeCanvas(w, r, principal, access.ActionView)
	if !ok {
		return
	}
	if a.Media == nil || a.Storage == nil {
		a.responder.Error(w, r, httpx.ErrMediaUnavailable, errMediaUnavailable)
		return
	}
	asset, err := a.Media.ReadyReference(r.Context(), mediaKey(key), r.PathValue("referenceID"))
	switch {
	case err == nil && (asset.Kind == media.KindImage || asset.Kind == media.KindGIF):
	case err == nil, errors.Is(err, media.ErrNotFound), errors.Is(err, media.ErrInvalid):
		a.mediaNotFound(w, r, key, media.ErrNotFound)
		return
	default:
		a.responder.Error(w, r, httpx.ErrInternal, err)
		return
	}
	etag := strconv.Quote(asset.ID)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		a.deliverWith(w, r, principal, key, func() {
			w.Header().Set("ETag", etag)
			httpx.Revalidate(w)
			w.WriteHeader(http.StatusNotModified)
		})
		return
	}
	body, err := a.Storage.Open(r.Context(), asset.StorageKey)
	if err != nil {
		// A ready asset without its bytes is a reconciliation finding.
		a.responder.Error(w, r, httpx.ErrInternal, fmt.Errorf("open ready asset: %w", err))
		return
	}
	defer body.Close()
	head := make([]byte, min(sniffBytes, asset.DeclaredBytes))
	if _, err := io.ReadFull(body, head); err != nil {
		a.responder.Error(w, r, httpx.ErrInternal, fmt.Errorf("read ready asset: %w", err))
		return
	}
	mediaType := http.DetectContentType(head)
	if !imageTypes[mediaType] || (mediaType == "image/gif") != (asset.Kind == media.KindGIF) {
		a.responder.Error(w, r, httpx.ErrInternal, errors.New("stored bytes are not an image of the asset's kind"))
		return
	}
	a.deliverWith(w, r, principal, key, func() {
		a.writeContent(w, r, key, asset, mediaType, head, body)
	})
}

// writeContent writes the image answer: headers, then the sniffed head and the
// rest of the stored bytes, nothing for HEAD.
func (a *api) writeContent(w http.ResponseWriter, r *http.Request, key canvas.Key, asset media.Asset, mediaType string, head []byte, body io.Reader) {
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Content-Length", strconv.FormatInt(asset.DeclaredBytes, 10))
	h.Set("ETag", strconv.Quote(asset.ID))
	httpx.Revalidate(w)
	a.Logger.LogAttrs(r.Context(), slog.LevelInfo, "media content served",
		slog.String("tenant_id", key.TenantID),
		slog.String("canvas_id", key.CanvasID),
		slog.String("asset_id", asset.ID),
	)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	rest := io.LimitReader(body, asset.DeclaredBytes-int64(len(head)))
	if _, err := io.Copy(w, io.MultiReader(bytes.NewReader(head), rest)); err != nil {
		// The status is sent; the client sees a short body.
		a.Logger.LogAttrs(r.Context(), slog.LevelWarn, "media content interrupted",
			slog.String("tenant_id", key.TenantID),
			slog.String("canvas_id", key.CanvasID),
			slog.String("asset_id", asset.ID),
			slog.Any("error", err),
		)
	}
}

// etagMatches reports whether an If-None-Match header names etag, weakly
// compared, or is the wildcard.
func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
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
