package route

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
)

// --- response shapes --------------------------------------------------------

type mergeRequestResponse struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`

	BranchID   int64  `json:"branch_id"`
	BranchName string `json:"branch,omitempty"`

	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`

	// ApprovedBy and ApprovedAt are null until somebody approves, and go back to
	// null when a later branch edit invalidates the approval. That reversal is
	// the visible consequence of the staleness rule, so it must be expressible.
	ApprovedBy *string `json:"approved_by"`
	ApprovedAt *string `json:"approved_at"`
	MergedAt   *string `json:"merged_at"`
}

func newMergeRequestResponse(m repository.MergeRequest, branchName string) mergeRequestResponse {
	return mergeRequestResponse{
		ID:         m.ID,
		Title:      m.Title,
		Status:     m.Status,
		BranchID:   m.BranchID,
		BranchName: branchName,
		CreatedBy:  m.CreatedBy,
		CreatedAt:  formatTime(m.CreatedAt),
		ApprovedBy: m.ApprovedBy,
		ApprovedAt: formatTimePtr(m.ApprovedAt),
		MergedAt:   formatTimePtr(m.MergedAt),
	}
}

type mergeRequestListResponse struct {
	MergeRequests []mergeRequestResponse `json:"merge_requests"`
}

type mergeRequestEventResponse struct {
	ID      int64  `json:"id"`
	Event   string `json:"event"`
	Comment string `json:"comment,omitempty"`
	// Actor is "system" for automatic transitions such as an approval
	// invalidated by a later branch edit.
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

type mergeRequestDetailResponse struct {
	MergeRequest mergeRequestResponse        `json:"merge_request"`
	Branch       branchResponse              `json:"branch"`
	Events       []mergeRequestEventResponse `json:"events"`
	Conflicts    conflictsResponse           `json:"conflicts"`
}

// valueConflictResponse is one (key, locale) both sides changed.
//
// Both sides carry three-state, and for the same reason the branch diff does: a
// reviewer choosing between "mine" and "master" must be able to see that one of
// the options is "there is no translation".
type valueConflictResponse struct {
	KeyID   int64  `json:"key_id"`
	KeyName string `json:"key_name"`
	Locale  string `json:"locale"`

	// MineRemoved means the branch tombstones the pair; Mine is then null.
	MineRemoved bool    `json:"mine_removed"`
	Mine        *string `json:"mine,omitempty"`

	// TheirsTranslated false means master has no row at all.
	TheirsTranslated bool    `json:"theirs_translated"`
	Theirs           *string `json:"theirs,omitempty"`

	BaseMasterVersion int `json:"base_master_version"`
	MasterVersion     int `json:"master_version"`

	// Resolution is "mine", "master", or empty while undecided.
	Resolution string `json:"resolution,omitempty"`
}

type metaConflictResponse struct {
	KeyID int64 `json:"key_id"`

	MineName     string `json:"mine_name"`
	TheirsName   string `json:"theirs_name"`
	MineStatus   string `json:"mine_status"`
	TheirsStatus string `json:"theirs_status"`

	BaseMasterVersion int    `json:"base_master_version"`
	MasterVersion     int    `json:"master_version"`
	Resolution        string `json:"resolution,omitempty"`
}

// nameCollisionResponse is the third conflict type, and the only one carrying
// no resolution field.
//
// Both sides independently claimed a name, and idx_keys_name_active permits one
// active key per name. Choosing a side cannot fix that — one of them has to be
// renamed — so offering a resolution would be offering a button that does
// nothing.
type nameCollisionResponse struct {
	Name string `json:"name"`
	// BranchKeyID is null for a key created on the branch.
	BranchKeyID *int64 `json:"branch_key_id"`
	MasterKeyID int64  `json:"master_key_id"`
}

type conflictsResponse struct {
	Values     []valueConflictResponse `json:"values"`
	Meta       []metaConflictResponse  `json:"metadata"`
	Collisions []nameCollisionResponse `json:"name_collisions"`

	// Unresolved counts the decisions still outstanding. Collisions are excluded
	// because no decision would clear them.
	Unresolved int `json:"unresolved"`

	// Mergeable is ADVISORY. The merge re-computes all of this inside its own
	// transaction under an advisory lock, and that answer is the one that counts
	// — this one can be stale before the reviewer's finger leaves the button.
	Mergeable bool `json:"mergeable"`
}

func newConflictsResponse(c mrsvc.Conflicts) conflictsResponse {
	out := conflictsResponse{
		Values:     newValueConflictResponses(c.Values),
		Meta:       newMetaConflictResponses(c.Meta),
		Collisions: newCollisionResponses(c.Collisions),
		Unresolved: c.Unresolved,
		Mergeable:  c.Mergeable(),
	}
	return out
}

func newValueConflictResponses(conflicts []repository.Conflict) []valueConflictResponse {
	out := make([]valueConflictResponse, 0, len(conflicts))
	for _, c := range conflicts {
		row := valueConflictResponse{
			KeyID: c.KeyID, KeyName: c.KeyName, Locale: c.LocaleCode,
			MineRemoved:       c.MineRemoved,
			TheirsTranslated:  c.TheirsFound,
			BaseMasterVersion: c.BaseMasterVersion,
			MasterVersion:     c.MasterVersion,
			Resolution:        c.Resolution,
		}
		if !c.MineRemoved {
			mine := c.Mine
			row.Mine = &mine
		}
		if c.TheirsFound {
			theirs := c.Theirs
			row.Theirs = &theirs
		}
		out = append(out, row)
	}
	return out
}

func newMetaConflictResponses(conflicts []repository.MetaConflict) []metaConflictResponse {
	out := make([]metaConflictResponse, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, metaConflictResponse{
			KeyID: c.KeyID, MineName: c.MineName, TheirsName: c.TheirsName,
			MineStatus: c.MineStatus, TheirsStatus: c.TheirsStatus,
			BaseMasterVersion: c.BaseMasterVersion, MasterVersion: c.MasterVersion,
			Resolution: c.Resolution,
		})
	}
	return out
}

func newCollisionResponses(collisions []repository.NameCollision) []nameCollisionResponse {
	out := make([]nameCollisionResponse, 0, len(collisions))
	for _, c := range collisions {
		out = append(out, nameCollisionResponse{
			Name: c.Name, BranchKeyID: c.BranchKeyID, MasterKeyID: c.MasterKeyID,
		})
	}
	return out
}

type mergeResultResponse struct {
	ReleaseID      int64 `json:"release_id"`
	ReleaseVersion int64 `json:"release_version"`
	ValuesApplied  int   `json:"values_applied"`
	KeysApplied    int   `json:"keys_applied"`
	BundlesWritten int   `json:"bundles_written"`
}

// --- request shapes ---------------------------------------------------------

type createMergeRequestRequest struct {
	Branch string `json:"branch"`
	Title  string `json:"title"`
}

type reviewRequest struct {
	Comment string `json:"comment"`
}

type resolutionRequest struct {
	KeyID int64 `json:"key_id"`
	// Locale is omitted for a key-metadata conflict, which has no locale
	// dimension. Sending one there would record the decision against a locale
	// the conflict does not have.
	Locale     string `json:"locale"`
	Resolution string `json:"resolution"`
}

type resolutionsRequest struct {
	Resolutions []resolutionRequest `json:"resolutions"`
}

// --- handlers ---------------------------------------------------------------

// ListMergeRequests returns requests newest first.
//
//	GET /api/v1/merge-requests?status=open
//
// Viewer.
func (h *Handler) ListMergeRequests(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "status"); err != nil {
		h.badRequest(w, r, err)
		return
	}

	listings, err := h.mrSvc.List(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		h.portalError(w, r, "list merge requests", err)
		return
	}

	out := mergeRequestListResponse{
		MergeRequests: make([]mergeRequestResponse, 0, len(listings)),
	}
	for _, l := range listings {
		out.MergeRequests = append(out.MergeRequests,
			newMergeRequestResponse(l.MergeRequest, l.BranchName))
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// GetMergeRequest returns one request with its branch, timeline and conflicts.
//
//	GET /api/v1/merge-requests/{id}
//
// Viewer. The conflicts travel with it: a review screen that fetched them
// separately would render "ready to merge" for the moment before the second
// request answered, and that moment is when somebody clicks.
func (h *Handler) GetMergeRequest(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	detail, err := h.mrSvc.Get(r.Context(), id)
	if err != nil {
		h.portalError(w, r, "get merge request", err)
		return
	}

	events := make([]mergeRequestEventResponse, 0, len(detail.Events))
	for _, e := range detail.Events {
		events = append(events, mergeRequestEventResponse{
			ID: e.ID, Event: e.Event, Comment: e.Comment,
			Actor: e.Actor, CreatedAt: formatTime(e.CreatedAt),
		})
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, mergeRequestDetailResponse{
		MergeRequest: newMergeRequestResponse(detail.MergeRequest, detail.Branch.Name),
		Branch:       newBranchResponse(detail.Branch),
		Events:       events,
		Conflicts:    newConflictsResponse(detail.Conflicts),
	})
}

// CreateMergeRequest opens a review on a branch.
//
//	POST /api/v1/merge-requests
//	{"branch":"q3-campaign-copy","title":"Q3 campaign copy"}
//
// Editor: asking for review is not approving.
func (h *Handler) CreateMergeRequest(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body createMergeRequestRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}
	if body.Branch == "" {
		h.badRequest(w, r, errors.New("branch is required"))
		return
	}

	created, err := h.mrSvc.Create(r.Context(), body.Branch, body.Title,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "create merge request", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newMergeRequestResponse(created, body.Branch))
}

// ApproveMergeRequest signs off the diff.
//
//	POST /api/v1/merge-requests/{id}/approve
//
// Approver. The approval records WHEN, and any later edit to the branch
// re-opens the request automatically — you cannot get a diff approved and then
// quietly change it.
func (h *Handler) ApproveMergeRequest(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, mrsvc.ActionApprove)
}

// RequestMergeRequestChanges sends the work back with a reason.
//
//	POST /api/v1/merge-requests/{id}/request-changes
//	{"comment":"the Thai string overflows the button"}
//
// Approver. The comment is required: sending work back without saying what is
// wrong is not a review.
func (h *Handler) RequestMergeRequestChanges(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, mrsvc.ActionRequestChanges)
}

// RejectMergeRequest refuses the change.
//
//	POST /api/v1/merge-requests/{id}/reject
//
// Approver. Terminal, but the branch survives and a fresh request can be opened
// against it — which is exactly what the partial unique index's exclusion of
// terminal states is for.
func (h *Handler) RejectMergeRequest(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, mrsvc.ActionReject)
}

// ReopenMergeRequest returns a rejected or closed request to open.
//
//	POST /api/v1/merge-requests/{id}/reopen
//
// Editor: reopening asks for review again, it does not grant one. A second live
// request on the same branch is refused with 409.
func (h *Handler) ReopenMergeRequest(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, mrsvc.ActionReopen)
}

// CloseMergeRequest withdraws the request.
//
//	POST /api/v1/merge-requests/{id}/close
//
// Editor: withdrawing your own proposal is not a review decision.
func (h *Handler) CloseMergeRequest(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, mrsvc.ActionClose)
}

func (h *Handler) review(w http.ResponseWriter, r *http.Request, action string) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body reviewRequest
	// The comment is optional on four of the five transitions, so an empty body
	// is normal. A malformed one is not, and must not be silently dropped.
	if r.ContentLength != 0 {
		if err := decodePortalJSON(w, r, &body); err != nil {
			h.badRequest(w, r, err)
			return
		}
	}

	moved, err := h.mrSvc.Review(r.Context(), id, action, body.Comment,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, action+" merge request", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newMergeRequestResponse(moved, ""))
}

// MergeRequestConflicts returns everything blocking the merge.
//
//	GET /api/v1/merge-requests/{id}/conflicts
//
// Viewer. All three kinds, because they are genuinely different: values and
// metadata are resolved by choosing a side, and name collisions cannot be —
// one of the two keys has to be renamed first.
func (h *Handler) MergeRequestConflicts(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	conflicts, err := h.mrSvc.Conflicts(r.Context(), id)
	if err != nil {
		h.portalError(w, r, "read merge request conflicts", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newConflictsResponse(conflicts))
}

// PutMergeRequestResolutions records human decisions about conflicts.
//
//	PUT /api/v1/merge-requests/{id}/resolutions
//	{"resolutions":[{"key_id":12,"locale":"en-SG","resolution":"mine"},
//	                {"key_id":13,"resolution":"master"}]}
//
// Editor. A resolution with no locale decides a key-METADATA conflict, which
// has no locale dimension.
//
// The whole set lands in one transaction, and the recomputed conflict list comes
// back — so a reviewer sees what is left without a second request that could
// observe a different world.
func (h *Handler) PutMergeRequestResolutions(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body resolutionsRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	resolutions := make([]mrsvc.Resolution, 0, len(body.Resolutions))
	for _, res := range body.Resolutions {
		resolutions = append(resolutions, mrsvc.Resolution{
			KeyID: res.KeyID, LocaleCode: res.Locale, Choice: res.Resolution,
		})
	}

	remaining, err := h.mrSvc.Resolve(r.Context(), id, resolutions,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "record conflict resolutions", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newConflictsResponse(remaining))
}

// MergeMergeRequest folds the branch into master and cuts a release.
//
//	POST /api/v1/merge-requests/{id}/merge
//
// Approver — this is the act that puts copy in front of customers.
//
// Every refusal it can produce is a 409 carrying enough to act on. A stale
// approval, unresolved conflicts, a name collision and an unapproved request are
// all expected outcomes of a workflow, not faults: answering 500 for any of them
// would page an engineer because two translators edited the same string.
func (h *Handler) MergeMergeRequest(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	result, err := h.mrSvc.Merge(r.Context(), id,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.mergeError(w, r, err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, mergeResultResponse{
		ReleaseID:      result.ReleaseID,
		ReleaseVersion: result.ReleaseVersion,
		ValuesApplied:  result.ValuesApplied,
		KeysApplied:    result.KeysApplied,
		BundlesWritten: result.BundlesWritten,
	})
}

// unresolvedResponse is the 409 body when conflicts block a merge.
//
// It carries the conflicting ROWS, not a count. "There were 3 conflicts" tells a
// reviewer nothing they can act on, and a portal that had to re-fetch them would
// be showing a list computed after the merge attempt rather than the one that
// blocked it.
type unresolvedResponse struct {
	Error   string `json:"error"`
	Details string `json:"details"`

	Values []valueConflictResponse `json:"values"`
	Meta   []metaConflictResponse  `json:"metadata"`
}

type collisionResponseBody struct {
	Error   string `json:"error"`
	Details string `json:"details"`
	// Collisions cannot be resolved by choosing a side: one of the two keys has
	// to be renamed before this merge can proceed.
	Collisions []nameCollisionResponse `json:"name_collisions"`
}

// mergeError maps mergesvc's sentinels, adding the bodies the shared mapper
// cannot build.
func (h *Handler) mergeError(w http.ResponseWriter, r *http.Request, err error) {
	var unresolved *mergesvc.UnresolvedError
	if errors.As(err, &unresolved) {
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, unresolvedResponse{
			Error:   "unresolved_conflicts",
			Details: err.Error(),
			Values:  newValueConflictResponses(unresolved.Values),
			Meta:    newMetaConflictResponses(unresolved.Meta),
		})
		return
	}

	var collision *mergesvc.CollisionError
	if errors.As(err, &collision) {
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, collisionResponseBody{
			Error:      "name_collision",
			Details:    err.Error(),
			Collisions: newCollisionResponses(collision.Collisions),
		})
		return
	}

	h.portalError(w, r, "merge", err)
}
