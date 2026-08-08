package route

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/assetsvc"
)

// maxAssetBodyBytes bounds the JSON bodies on this router.
//
// These endpoints carry declarations, never image data — the bytes go straight
// to S3 — so a body larger than this is either a mistake or an attempt to make
// the process allocate.
const maxAssetBodyBytes = 8 << 10

type presignRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
}

type assetResponse struct {
	ID          int64  `json:"id"`
	SHA256      string `json:"sha256"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	UploadedBy  string `json:"uploaded_by"`
	CreatedAt   string `json:"created_at"`
}

// presignResponse carries exactly one of asset or upload.
//
// s3_key is exposed but the presigned URL and its form fields are the actual
// credential; nothing here is logged.
type presignResponse struct {
	// Deduplicated tells the client to skip the upload entirely. It is
	// explicit rather than implied by a nil upload, because "no work to do" and
	// "something went wrong" must not look alike to a browser.
	Deduplicated bool            `json:"deduplicated"`
	Asset        *assetResponse  `json:"asset,omitempty"`
	Upload       *uploadResponse `json:"upload,omitempty"`
}

type uploadResponse struct {
	// URL and Fields are a multipart/form-data POST target. The browser must
	// send every field verbatim: they are signed, and S3 rejects the upload if
	// any of them is altered or omitted.
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
	S3Key  string            `json:"s3_key"`
}

type confirmRequest struct {
	SHA256 string `json:"sha256"`
}

type attachRequest struct {
	AssetID int64  `json:"asset_id"`
	Note    string `json:"note"`
}

type urlResponse struct {
	URL string `json:"url"`
}

// AssetPresign validates a declaration and returns an upload form, or the
// existing asset when these exact bytes are already stored.
//
//	POST /api/v1/assets/presign
//	{"filename":"cart.png","content_type":"image/png","bytes":81234,"sha256":"<hex>"}
func (h *Handler) AssetPresign(w http.ResponseWriter, r *http.Request) {
	var body presignRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	result, err := h.assets.Presign(r.Context(), assetsvc.PresignRequest{
		Filename:    body.Filename,
		ContentType: body.ContentType,
		Bytes:       body.Bytes,
		SHA256:      body.SHA256,
	}, actorFromContext(r.Context()))
	if err != nil {
		h.assetError(w, r, "presign", err)
		return
	}

	rsp := presignResponse{}
	if result.Asset != nil {
		rsp.Deduplicated = true
		rsp.Asset = newAssetResponse(*result.Asset)
	} else {
		rsp.Upload = &uploadResponse{
			URL:    result.Upload.URL,
			Fields: result.Upload.Fields,
			S3Key:  result.Upload.S3Key,
		}
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, rsp)
}

// AssetConfirm records an asset after verifying the object is really there.
//
//	POST /api/v1/assets/confirm
//	{"sha256":"<hex>"}
//
// The body is only the hash on purpose. Everything else is read back from the
// object's own metadata, which S3 enforced against the signed policy, so there
// is no second client declaration that could disagree with the first.
func (h *Handler) AssetConfirm(w http.ResponseWriter, r *http.Request) {
	var body confirmRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	asset, err := h.assets.Confirm(r.Context(), body.SHA256,
		actorFromContext(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.assetError(w, r, "confirm", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newAssetResponse(asset))
}

// AssetURL issues a short-lived presigned GET.
//
//	GET /api/v1/assets/{id}/url
func (h *Handler) AssetURL(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	url, err := h.assets.SignedURL(r.Context(), id,
		actorFromContext(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.assetError(w, r, "sign asset url", err)
		return
	}

	// The URL is a bearer credential for customer PII. No cache, anywhere.
	w.Header().Set("Cache-Control", "no-store")
	render.Status(r, http.StatusOK)
	render.JSON(w, r, urlResponse{URL: url})
}

// AssetAttach links an asset to a key.
//
//	PUT /api/v1/keys/{id}/assets
//	{"asset_id":88,"note":"truncates past 18 characters"}
//
// PUT rather than POST because attaching is idempotent: repeating it with a
// different note amends the note rather than creating a second link.
func (h *Handler) AssetAttach(w http.ResponseWriter, r *http.Request) {
	keyID, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body attachRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}
	if body.AssetID <= 0 {
		h.badRequest(w, r, errors.New("asset_id is required"))
		return
	}

	err = h.assets.Attach(r.Context(), keyID, body.AssetID, body.Note,
		actorFromContext(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.assetError(w, r, "attach asset", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AssetDetach removes the link between a key and an asset.
//
//	DELETE /api/v1/keys/{id}/assets/{assetId}
//
// The asset itself survives: the bytes are content-addressed and another key
// may still reference them.
func (h *Handler) AssetDetach(w http.ResponseWriter, r *http.Request) {
	keyID, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	assetID, err := pathID(r, "assetId")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	err = h.assets.Detach(r.Context(), keyID, assetID,
		actorFromContext(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.assetError(w, r, "detach asset", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// assetError maps the service's sentinels onto status codes.
//
// Everything the caller can fix is a 4xx. Only an unrecognised error reaches
// 500, and only that case is logged as an error — a client confirming an upload
// that never happened is not an incident.
func (h *Handler) assetError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, assetsvc.ErrBadRequest):
		h.badRequest(w, r, err)

	case errors.Is(err, repository.ErrNotFound):
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, errorResponse{Error: "not_found", Details: err.Error()})

	case errors.Is(err, assetsvc.ErrUploadNotFound):
		// 409, not 404: the asset id in the URL is fine, the state the client
		// believes it is in is not. A 404 would send a browser looking for a
		// missing endpoint.
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "upload_not_found", Details: err.Error()})

	case errors.Is(err, assetsvc.ErrUploadMismatch), errors.Is(err, assetsvc.ErrUploadUnverifiable):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "upload_mismatch", Details: err.Error()})

	default:
		log.Errore(r.Context(), "asset "+op+" failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "internal_error"})
	}
}

func newAssetResponse(a repository.Asset) *assetResponse {
	return &assetResponse{
		ID:          a.ID,
		SHA256:      a.SHA256,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		Bytes:       a.Bytes,
		UploadedBy:  a.UploadedBy,
		CreatedAt:   a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// decodeJSON reads a bounded, strict JSON body on the asset routes.
//
// DisallowUnknownFields for the same reason parseExportRequest rejects unknown
// query parameters: a client that misspells "content_type" must be told, not
// silently handed a zero value and a confusing validation error two layers
// down. See decodeJSONLimit in portal.go — the asset limit is deliberately
// tighter than the portal's, because these bodies carry declarations rather
// than copy.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSONLimit(w, r, dst, maxAssetBodyBytes)
}

func pathID(r *http.Request, param string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New(param + " must be a positive integer")
	}
	return id, nil
}

// actorFromContext names who is acting, for uploaded_by and audit_events.
//
// Until the portal identities of Phase 5 exist, the only authenticated
// principal is an API token, so the token's NAME is the actor. It is prefixed
// so a later portal user called "ci" can never be confused with the token
// called "ci" in an audit query — an audit trail whose actors are ambiguous
// answers nothing.
//
// The fallback can only be reached if one of these routes is ever mounted
// outside RequireAPIToken. It is a legible value rather than an empty string
// because uploaded_by and actor are NOT NULL, and a blank actor in an audit row
// is worse than an honest "unauthenticated".
func actorFromContext(ctx context.Context) string {
	if token, ok := TokenFromContext(ctx); ok {
		return "token:" + token.Name
	}
	return "unauthenticated"
}
