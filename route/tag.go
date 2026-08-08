package route

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// --- response shapes --------------------------------------------------------

// tagUsageResponse is a tag plus how many keys carry it.
//
// KeyCount is only ever populated by the list, which is why it lives on this
// type and not on tagResponse: a zero on a tag returned from anywhere else would
// read as "no keys have this" when it actually means "nobody counted".
type tagUsageResponse struct {
	ID       int16  `json:"id"`
	Name     string `json:"name"`
	Colour   string `json:"colour"`
	KeyCount int    `json:"key_count"`
	// CreatedAt is present because the tag manager sorts by it when two tags
	// have the same usage.
	CreatedAt string `json:"created_at"`
}

type tagListResponse struct {
	Tags []tagUsageResponse `json:"tags"`
}

type keyTagsResponse struct {
	KeyID int64         `json:"key_id"`
	Tags  []tagResponse `json:"tags"`
}

// bulkTagResponse reports what a bulk assign or unassign actually did.
//
// Removed is present only for an unassign. "Detached 40 of the 50 you asked
// for" is a different fact from "detached all 50" — the caller selected a page
// of keys and not all of them carried the tag — and the portal shows it. An
// assign has no equivalent number: the insert is ON CONFLICT DO NOTHING, so a
// retried request would report "0 added" and read as a failure.
type bulkTagResponse struct {
	Tag       tagResponse `json:"tag"`
	Requested int         `json:"requested"`
	Removed   *int64      `json:"removed,omitempty"`
}

// deleteTagResponse reports the cascade.
//
// The count is returned rather than a bare 204 because this delete is
// destructive beyond the row addressed: key_tags.tag_id cascades, so "deleted
// the tag" and "deleted the tag and detached it from 812 keys" are very
// different outcomes for the person who clicked.
type deleteTagResponse struct {
	DetachedKeys int `json:"detached_keys"`
}

// --- request shapes ---------------------------------------------------------

type tagRequest struct {
	Name   string `json:"name"`
	Colour string `json:"colour"`
}

type setKeyTagsRequest struct {
	// TagIDs is the complete set the key should END UP with, not a diff. An
	// empty array clears every tag, and that is the documented way to say "no
	// tags" — without it there would be no way to remove the last one.
	//
	// A pointer so an omitted field is refused rather than silently read as
	// "clear everything".
	TagIDs *[]int16 `json:"tag_ids"`
}

type bulkTagRequest struct {
	KeyIDs []int64 `json:"key_ids"`
}

// --- handlers ---------------------------------------------------------------

// ListTags returns every tag with its usage count.
//
//	GET /api/v1/tags
//
// Viewer. One query, counts included: the tag manager renders the whole list,
// and a count per row would be one round trip per row.
func (h *Handler) ListTags(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	usages, err := h.tagSvc.List(r.Context())
	if err != nil {
		h.portalError(w, r, "list tags", err)
		return
	}

	out := tagListResponse{Tags: make([]tagUsageResponse, 0, len(usages))}
	for _, u := range usages {
		out.Tags = append(out.Tags, tagUsageResponse{
			ID: u.ID, Name: u.Name, Colour: u.Colour,
			KeyCount: u.KeyCount, CreatedAt: formatTime(u.CreatedAt),
		})
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// CreateTag adds a workflow label.
//
//	POST /api/v1/tags
//	{"name":"needs-review","colour":"#f0a000"}
//
// Editor.
func (h *Handler) CreateTag(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body tagRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	created, err := h.tagSvc.Create(r.Context(), body.Name, body.Colour,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "create tag", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, tagResponse{ID: created.ID, Name: created.Name, Colour: created.Colour})
}

// UpdateTag renames or recolours a tag.
//
//	PUT /api/v1/tags/{id}
//	{"name":"reviewed","colour":"#00a000"}
//
// Editor. PUT rather than PATCH: both fields are always sent, because a tag has
// only two and a partial update of two fields is ceremony without benefit.
//
// A rename rewrites what every key carrying the tag appears to claim, so the
// audit row records the previous name.
func (h *Handler) UpdateTag(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathTagID(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body tagRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	updated, err := h.tagSvc.Update(r.Context(), id, body.Name, body.Colour,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "update tag", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, tagResponse{ID: updated.ID, Name: updated.Name, Colour: updated.Colour})
}

// DeleteTag removes a tag and detaches it from every key.
//
//	DELETE /api/v1/tags/{id}
//
// Editor. DESTRUCTIVE BEYOND THE ROW ADDRESSED: key_tags.tag_id cascades, and
// key_tags has no history table, so the fact that any key ever carried this tag
// disappears with it. The audit row records the affected key ids first, inside
// the same transaction, which is the only thing that makes the deletion
// reconstructable.
//
// The response carries the number of keys detached rather than a bare 204,
// because "deleted the tag" and "deleted the tag and detached it from 812 keys"
// are very different outcomes for the person who clicked.
func (h *Handler) DeleteTag(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathTagID(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	detached, err := h.tagSvc.Delete(r.Context(), id,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "delete tag", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, deleteTagResponse{DetachedKeys: detached})
}

// PutKeyTags replaces a key's complete tag set.
//
//	PUT /api/v1/keys/{id}/tags
//	{"tag_ids":[1,4]}
//
// Editor. The body is what the key should END UP with, not a diff — an empty
// array clears every tag, which is the documented way to say "no tags".
func (h *Handler) PutKeyTags(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	keyID, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body setKeyTagsRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}
	if body.TagIDs == nil {
		// An omitted field must not be read as "clear everything". Clearing is a
		// deliberate act and it has its own spelling: [].
		h.badRequest(w, r, errors.New(
			"tag_ids is required; send [] to remove every tag from this key"))
		return
	}

	applied, err := h.tagSvc.SetKeyTags(r.Context(), keyID, *body.TagIDs,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "set key tags", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, keyTagsResponse{KeyID: keyID, Tags: newTagResponses(applied)})
}

// AssignTag attaches one tag to many keys.
//
//	POST /api/v1/tags/{id}/keys
//	{"key_ids":[12,13,14]}
//
// Editor. Idempotent: keys already carrying the tag keep their original
// created_at rather than looking newly tagged.
func (h *Handler) AssignTag(w http.ResponseWriter, r *http.Request) {
	h.bulkTag(w, r, true)
}

// UnassignTag detaches one tag from many keys.
//
//	DELETE /api/v1/tags/{id}/keys
//	{"key_ids":[12,13,14]}
//
// Editor. The response reports how many links actually went away, which is
// routinely fewer than requested — the caller selected a page of keys and not
// all of them carried the tag.
func (h *Handler) UnassignTag(w http.ResponseWriter, r *http.Request) {
	h.bulkTag(w, r, false)
}

func (h *Handler) bulkTag(w http.ResponseWriter, r *http.Request, add bool) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathTagID(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body bulkTagRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	op := "unassign tag"
	action := h.tagSvc.RemoveFromKeys
	if add {
		op = "assign tag"
		action = h.tagSvc.AddToKeys
	}

	result, err := action(r.Context(), id, body.KeyIDs,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, op, err)
		return
	}

	out := bulkTagResponse{
		Tag:       tagResponse{ID: result.Tag.ID, Name: result.Tag.Name, Colour: result.Tag.Colour},
		Requested: result.Requested,
	}
	if !add {
		removed := result.Removed
		out.Removed = &removed
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// pathTagID reads the {id} path parameter as a tags.id.
//
// tags.id is a SMALLSERIAL, so the range check is not pedantry: a value above
// 32767 would overflow silently on the way to the database and address a
// different tag, or none.
func pathTagID(r *http.Request) (int16, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 16)
	if err != nil || id <= 0 {
		return 0, errors.New("tag id must be a positive integer below 32768")
	}
	return int16(id), nil
}

var _ = repository.Tag{}
