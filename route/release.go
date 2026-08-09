package route

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/releasesvc"
)

// --- response shapes --------------------------------------------------------

type releaseResponse struct {
	ID      int64  `json:"id"`
	Version int64  `json:"version"`
	Source  string `json:"source"`
	Notes   string `json:"notes,omitempty"`

	// MergeRequestID is null for a manual publish or an import. That null is the
	// difference between "somebody approved this" and "somebody pushed it", and
	// it is the first thing an incident review looks at.
	MergeRequestID *int64 `json:"merge_request_id"`

	// MinAppVersion is null when every client is eligible.
	MinAppVersion *string `json:"min_app_version"`

	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`

	// RolledBack and its two fields are the kill switch. They are set together
	// or not at all — the schema enforces it — and rolled_back_by is the answer
	// to the only question anybody asks afterwards.
	RolledBack   bool    `json:"rolled_back"`
	RolledBackAt *string `json:"rolled_back_at"`
	RolledBackBy *string `json:"rolled_back_by"`

	LocaleCount int `json:"locale_count"`
	KeyCount    int `json:"key_count"`
}

func newReleaseResponse(d repository.ReleaseDetail) releaseResponse {
	return releaseResponse{
		ID:             d.ID,
		Version:        d.Version,
		Source:         d.Source,
		Notes:          d.Notes,
		MergeRequestID: d.MergeRequestID,
		MinAppVersion:  d.MinAppVersion,
		CreatedBy:      d.CreatedBy,
		CreatedAt:      formatTime(d.CreatedAt),
		RolledBack:     d.RolledBack(),
		RolledBackAt:   formatTimePtr(d.RolledBackAt),
		RolledBackBy:   d.RolledBackBy,
		LocaleCount:    d.LocaleCount,
		KeyCount:       d.KeyCount,
	}
}

type releaseListResponse struct {
	Releases []releaseResponse `json:"releases"`
	Limit    int               `json:"limit"`
	Offset   int               `json:"offset"`
}

type bundleResponse struct {
	Version int64  `json:"version"`
	Locale  string `json:"locale"`
	// SHA256 is the content fingerprint that doubles as the OTA ETag, so the
	// portal can confirm a client is on exactly this bundle.
	SHA256   string `json:"sha256"`
	KeyCount int    `json:"key_count"`
	ByteSize int    `json:"byte_size"`
	// Strings is passed through as stored rather than decoded and re-encoded:
	// re-encoding a 60KB document would change nothing except the risk of
	// changing something.
	Strings json.RawMessage `json:"strings"`
}

// --- request shapes ---------------------------------------------------------

type publishRequest struct {
	Notes string `json:"notes"`
	// MinAppVersion withholds the release from older clients. Exactly three
	// numeric components — see releasesvc.validateMinAppVersion for why "4.12"
	// is refused rather than accepted.
	MinAppVersion string `json:"min_app_version"`
}

// --- handlers ---------------------------------------------------------------

// ListReleases returns release history, newest first.
//
//	GET /api/v1/releases?limit=50&offset=0
//
// Viewer: knowing what shipped is reading.
func (h *Handler) ListReleases(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "limit", "offset"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", releasesvc.DefaultListLimit)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	releases, err := h.releaseSvc.List(r.Context(), limit, offset)
	if err != nil {
		h.portalError(w, r, "list releases", err)
		return
	}

	out := releaseListResponse{
		Releases: make([]releaseResponse, 0, len(releases)),
		Limit:    limit,
		Offset:   offset,
	}
	for _, d := range releases {
		out.Releases = append(out.Releases, newReleaseResponse(d))
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// GetRelease returns one release.
//
//	GET /api/v1/releases/{version}
func (h *Handler) GetRelease(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	release, err := h.releaseSvc.Get(r.Context(), version)
	if err != nil {
		h.portalError(w, r, "get release", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newReleaseResponse(release))
}

// PublishRelease cuts a release from master's current state.
//
//	POST /api/v1/releases
//	{"notes":"typo fix in the top-up sheet","min_app_version":"4.12.0"}
//
// Approver, and the one path that puts copy in front of customers with no diff
// reviewed. It exists because master can be correct while no merge request is
// open — after an import, or a direct fix — and because a rollback needs
// something to roll forward to.
//
// The release row and all six bundles land in one transaction: a release that
// became servable a moment before its bundles existed would hand an OTA client
// an empty document behind an ETag, and the client would cache it and stop
// asking.
func (h *Handler) PublishRelease(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body publishRequest
	if r.ContentLength != 0 {
		if err := decodePortalJSON(w, r, &body); err != nil {
			h.badRequest(w, r, err)
			return
		}
	}

	published, err := h.releaseSvc.Publish(r.Context(), body.Notes, body.MinAppVersion,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "publish release", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newReleaseResponse(published))
}

// RollbackRelease is the OTA kill switch.
//
//	POST /api/v1/releases/{version}/rollback
//
// Approver. It withholds the release from serving; clients fall back to the
// newest earlier eligible release, or — when there is none — receive 410 and use
// the strings compiled into the app binary.
//
// It does NOT undo the values. master keeps whatever the merge applied, and only
// the SERVED bundle changes. Undoing the data is a separate act, because a
// rollback happens in a hurry and must not also rewrite the corpus.
//
// Rolling back an already-rolled-back release is 409, not a repeat: the stored
// rolled_back_by answers the only question anybody asks afterwards, and a second
// write would replace that name with whoever pressed the button last.
func (h *Handler) RollbackRelease(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	rolled, err := h.releaseSvc.Rollback(r.Context(), version,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "roll back release", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newReleaseResponse(rolled))
}

// ReleaseBundle returns exactly what a release shipped for one locale.
//
//	GET /api/v1/releases/{version}/bundles/{locale}
//
// Viewer. It reads the MATERIALISED bundle rather than re-deriving it from
// master: the whole point of materialising at merge time is that the answer to
// "what did release 41 ship?" cannot drift afterwards.
func (h *Handler) ReleaseBundle(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	locale := chi.URLParam(r, "locale")
	if locale == "" {
		h.badRequest(w, r, errors.New("locale is required"))
		return
	}

	bundle, err := h.releaseSvc.Bundle(r.Context(), version, locale)
	if err != nil {
		h.portalError(w, r, "read release bundle", err)
		return
	}

	// A release is immutable, so its bundle can be cached hard — but this is the
	// portal's copy, behind an identity, and caching it in a shared proxy would
	// serve one operator's authenticated response to another.
	w.Header().Set("Cache-Control", "private, max-age=300")

	render.Status(r, http.StatusOK)
	render.JSON(w, r, bundleResponse{
		Version:  bundle.ReleaseVersion,
		Locale:   bundle.LocaleCode,
		SHA256:   bundle.SHA256,
		KeyCount: bundle.KeyCount,
		ByteSize: bundle.ByteSize,
		Strings:  json.RawMessage(bundle.Strings),
	})
}

// pathVersion reads the {version} path parameter.
//
// Releases are addressed by their human-facing VERSION rather than their row id,
// because that is the number in the incident channel when somebody says "roll
// back 41".
func pathVersion(r *http.Request) (int64, error) {
	version, err := strconv.ParseInt(chi.URLParam(r, "version"), 10, 64)
	if err != nil || version <= 0 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}
