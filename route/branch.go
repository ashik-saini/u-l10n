package route

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// --- response shapes --------------------------------------------------------

type branchResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`

	// LastEditedAt is null for a branch nobody has written to yet. It is what
	// invalidates an approval, so the portal shows it next to the review state.
	LastEditedAt *string `json:"last_edited_at"`
	MergedAt     *string `json:"merged_at"`

	ValueChanges int `json:"value_changes"`
	MetaChanges  int `json:"meta_changes"`

	// MergeRequestID is null when the branch has no LIVE request — which is a
	// different fact from having had one that was rejected.
	MergeRequestID     *int64 `json:"merge_request_id"`
	MergeRequestStatus string `json:"merge_request_status,omitempty"`
}

func newBranchResponse(s repository.BranchSummary) branchResponse {
	return branchResponse{
		ID:                 s.ID,
		Name:               s.Name,
		Description:        s.Description,
		Status:             s.Status,
		CreatedBy:          s.CreatedBy,
		CreatedAt:          formatTime(s.CreatedAt),
		LastEditedAt:       formatTimePtr(s.LastEditedAt),
		MergedAt:           formatTimePtr(s.MergedAt),
		ValueChanges:       s.ValueChanges,
		MetaChanges:        s.MetaChanges,
		MergeRequestID:     s.MergeRequestID,
		MergeRequestStatus: s.MergeRequestStatus,
	}
}

// newBranchResponseFromBranch is the shape returned by the write endpoints,
// where the counts have not been read.
//
// The counts are omitted rather than reported as zero: "this branch changes
// nothing" and "nobody counted" are different claims, and a create response
// asserting the former would be wrong the moment the first edit lands.
func newBranchResponseFromBranch(b repository.Branch) branchResponse {
	return newBranchResponse(repository.BranchSummary{Branch: b})
}

type branchListResponse struct {
	Branches []branchResponse `json:"branches"`
}

// valueChangeResponse is one changed cell in a branch diff.
//
// Both sides carry the three-state rule. Removed means the branch tombstones
// the pair — the merge DELETEs master's row rather than blanking it — and
// MasterTranslated false means master has no row at all. A diff that showed
// both as "" would give a reviewer no way to tell a deletion from a blanking.
type valueChangeResponse struct {
	KeyID   int64  `json:"key_id"`
	KeyName string `json:"key_name"`
	Locale  string `json:"locale"`

	Removed bool    `json:"removed"`
	Value   *string `json:"value,omitempty"`

	MasterTranslated bool    `json:"master_translated"`
	MasterValue      *string `json:"master_value,omitempty"`

	BaseMasterVersion int `json:"base_master_version"`
	MasterVersion     int `json:"master_version"`

	// Conflict is the merge's own rule: master moved since this branch first
	// touched the pair.
	Conflict bool `json:"conflict"`

	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

type metaChangeResponse struct {
	// KeyID always names a real key. A key created on this branch has one too:
	// it exists as a draft until the merge promotes it, which is what
	// master_status = "draft" on this row reports.
	KeyID int64 `json:"key_id"`

	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`

	MasterName   string `json:"master_name,omitempty"`
	MasterStatus string `json:"master_status,omitempty"`

	BaseMasterVersion int  `json:"base_master_version"`
	MasterVersion     int  `json:"master_version"`
	Conflict          bool `json:"conflict"`

	UpdatedBy string `json:"updated_by"`
	UpdatedAt string `json:"updated_at"`
}

type branchChangesResponse struct {
	Branch string `json:"branch"`
	Status string `json:"status"`

	Values []valueChangeResponse `json:"values"`
	Meta   []metaChangeResponse  `json:"meta"`

	// Conflicts counts the flagged rows across both lists, so the portal can
	// render "3 conflicts" without walking them.
	Conflicts int `json:"conflicts"`
}

func newBranchChangesResponse(b repository.Branch, c repository.BranchChanges) branchChangesResponse {
	out := branchChangesResponse{
		Branch: b.Name,
		Status: b.Status,
		Values: make([]valueChangeResponse, 0, len(c.Values)),
		Meta:   make([]metaChangeResponse, 0, len(c.Meta)),
	}

	for _, v := range c.Values {
		row := valueChangeResponse{
			KeyID: v.KeyID, KeyName: v.KeyName, Locale: v.LocaleCode,
			Removed:           v.Removed,
			MasterTranslated:  v.MasterFound,
			BaseMasterVersion: v.BaseMasterVersion,
			MasterVersion:     v.MasterVersion,
			Conflict:          v.Conflict,
			UpdatedBy:         v.UpdatedBy,
			UpdatedAt:         formatTime(v.UpdatedAt),
		}
		if !v.Removed {
			value := v.Value
			row.Value = &value
		}
		if v.MasterFound {
			master := v.MasterValue
			row.MasterValue = &master
		}
		if v.Conflict {
			out.Conflicts++
		}
		out.Values = append(out.Values, row)
	}

	for _, m := range c.Meta {
		out.Meta = append(out.Meta, metaChangeResponse{
			KeyID: m.KeyID, Name: m.Name, Description: m.Description,
			Status: m.Status, MasterName: m.MasterName, MasterStatus: m.MasterStatus,
			BaseMasterVersion: m.BaseMasterVersion, MasterVersion: m.MasterVersion,
			Conflict: m.Conflict, UpdatedBy: m.UpdatedBy,
			UpdatedAt: formatTime(m.UpdatedAt),
		})
		if m.Conflict {
			out.Conflicts++
		}
	}
	return out
}

type createBranchRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// --- handlers ---------------------------------------------------------------

// ListBranches returns every branch with its delta counts.
//
//	GET /api/v1/branches?status=open
//
// Viewer: seeing what work is in flight is reading, not writing.
func (h *Handler) ListBranches(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "status"); err != nil {
		h.badRequest(w, r, err)
		return
	}

	summaries, err := h.branchSvc.List(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		h.portalError(w, r, "list branches", err)
		return
	}

	out := branchListResponse{Branches: make([]branchResponse, 0, len(summaries))}
	for _, s := range summaries {
		out.Branches = append(out.Branches, newBranchResponse(s))
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// GetBranch returns one branch.
//
//	GET /api/v1/branches/{name}
func (h *Handler) GetBranch(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	name, err := pathBranchName(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	summary, err := h.branchSvc.Get(r.Context(), name)
	if err != nil {
		h.portalError(w, r, "get branch", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newBranchResponse(summary))
}

// CreateBranch opens a workspace.
//
//	POST /api/v1/branches
//	{"name":"q3-campaign-copy","description":"..."}
//
// Editor. A branch stores only deltas, so this is one INSERT and costs nothing
// — which is what makes "branch first, review later" a reasonable default.
func (h *Handler) CreateBranch(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body createBranchRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	created, err := h.branchSvc.Create(r.Context(), body.Name, body.Description,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "create branch", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newBranchResponseFromBranch(created))
}

// CloseBranch abandons a branch without merging it.
//
//	POST /api/v1/branches/{name}/close
//
// Editor. The deltas survive: a closed branch records a decision not to ship
// those changes, and deleting it would make that decision unauditable.
func (h *Handler) CloseBranch(w http.ResponseWriter, r *http.Request) {
	h.branchTransition(w, r, h.branchSvc.Close, "close branch")
}

// ReopenBranch returns a closed branch to editable.
//
//	POST /api/v1/branches/{name}/reopen
//
// Editor. A MERGED branch is refused with 409: its deltas are the permanent
// record of what the merge applied.
func (h *Handler) ReopenBranch(w http.ResponseWriter, r *http.Request) {
	h.branchTransition(w, r, h.branchSvc.Reopen, "reopen branch")
}

func (h *Handler) branchTransition(
	w http.ResponseWriter, r *http.Request,
	action func(ctx context.Context, name, actor, requestID string) (repository.Branch, error),
	op string,
) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	name, err := pathBranchName(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	moved, err := action(r.Context(), name,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, op, err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newBranchResponseFromBranch(moved))
}

// BranchChanges returns the branch's diff against master.
//
//	GET /api/v1/branches/{name}/changes
//
// Viewer. Every row carries a conflict flag computed with the merge's own
// version comparison — a diff that disagreed with the merge would have a
// reviewer approve a clean-looking change and watch the merge refuse it.
func (h *Handler) BranchChanges(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	name, err := pathBranchName(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	branch, changes, err := h.branchSvc.Changes(r.Context(), name)
	if err != nil {
		h.portalError(w, r, "read branch changes", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newBranchChangesResponse(branch, changes))
}

// pathBranchName reads the {name} path parameter.
//
// Branch names are constrained at creation to characters that survive a URL
// path segment, so nothing here needs unescaping — but an empty segment is
// still worth naming, because chi will happily route one.
func pathBranchName(r *http.Request) (string, error) {
	name := chi.URLParam(r, "name")
	if name == "" {
		return "", errors.New("branch name is required")
	}
	return name, nil
}
