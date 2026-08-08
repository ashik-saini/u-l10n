package route

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
)

// maxBrowseLimit caps one page of the key browser.
//
// It has to be larger than the corpus — ~6,300 keys — because the browser
// genuinely fetches the whole thing in one request, and a cap below that would
// silently truncate the page rather than refuse it. It exists only to stop a
// caller asking for a number so large the response cannot be assembled.
const maxBrowseLimit = 20000

// --- response shapes --------------------------------------------------------

// cellResponse is one (key, locale) value, and it is where the three-state rule
// meets JSON.
//
//	{"translated": false}                      no row: untranslated
//	{"translated": true,  "value": ""}         a row holding "": deliberately blank
//	{"translated": true,  "value": "Top up"}   translated
//
// Value is a *string with omitempty precisely so a pointer to "" serialises as
// "value":"" while a nil pointer disappears. A plain string would render both of
// the first two cases as "value":"" and collapse the distinction that ~430
// en-SG keys and 3,664 ms-MY values depend on.
type cellResponse struct {
	Translated bool    `json:"translated"`
	Value      *string `json:"value,omitempty"`
	RenderHint string  `json:"render_hint,omitempty"`

	// Version is MASTER's version and the value to send back as base_version.
	// Zero means there is no master row, which is a legitimate base.
	Version int `json:"version"`

	// FromBranch marks a cell the branch overrides. The portal renders those
	// differently, and without it a branch view is indistinguishable from
	// master.
	FromBranch bool `json:"from_branch"`

	UpdatedBy string  `json:"updated_by,omitempty"`
	UpdatedAt *string `json:"updated_at,omitempty"`
}

func newCellResponse(c repository.Cell) cellResponse {
	out := cellResponse{
		Translated: c.Found,
		Version:    c.Version,
		FromBranch: c.FromBranch,
		UpdatedBy:  c.UpdatedBy,
		UpdatedAt:  formatTimePtr(c.UpdatedAt),
	}
	if c.Found {
		value := c.Value
		out.Value = &value
		out.RenderHint = string(c.RenderHint)
	}
	return out
}

type tagResponse struct {
	ID     int16  `json:"id"`
	Name   string `json:"name"`
	Colour string `json:"colour"`
}

func newTagResponses(tags []repository.Tag) []tagResponse {
	// A non-nil empty slice, so "this key has no tags" serialises as [] rather
	// than null and the portal does not need a null check per row.
	out := make([]tagResponse, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagResponse{ID: t.ID, Name: t.Name, Colour: t.Colour})
	}
	return out
}

type keyResponse struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platforms   []string `json:"platforms"`

	// AndroidName and IOSName stay nullable: NULL means "derive from the key
	// name" and a string means the derivation was deliberately overridden.
	AndroidName *string `json:"android_name"`
	IOSName     *string `json:"ios_name"`

	Status    string `json:"status"`
	Version   int    `json:"version"`
	SortIndex int64  `json:"sort_index"`

	Tags []tagResponse `json:"tags"`

	// Values is keyed on locale CODE and holds an entry for every requested
	// locale, translated or not.
	Values map[string]cellResponse `json:"values"`

	// BranchModified reports that the branch overrides this key's metadata.
	BranchModified bool `json:"branch_modified"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func newKeyResponse(v keysvc.KeyView, locales []model.Locale) keyResponse {
	out := keyResponse{
		ID:             v.Key.ID,
		Name:           v.Key.Name,
		Description:    v.Key.Description,
		Platforms:      make([]string, 0, len(v.Key.Platforms)),
		AndroidName:    v.Key.AndroidName,
		IOSName:        v.Key.IOSName,
		Status:         string(v.Key.Status),
		Version:        v.Key.Version,
		SortIndex:      v.Key.SortIndex,
		Tags:           newTagResponses(v.Tags),
		Values:         make(map[string]cellResponse, len(locales)),
		BranchModified: v.BranchModified,
		CreatedAt:      formatTime(v.Key.CreatedAt),
		UpdatedAt:      formatTime(v.Key.UpdatedAt),
	}
	for _, p := range v.Key.Platforms {
		out.Platforms = append(out.Platforms, string(p))
	}
	for _, l := range locales {
		out.Values[l.Code] = newCellResponse(v.Values[l.ID])
	}
	return out
}

type keyListResponse struct {
	Keys []keyResponse `json:"keys"`
	// Total is the size of the whole matching set, not of this page.
	Total   int      `json:"total"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
	Locales []string `json:"locales"`
	// Branch is null on master, so a caller can always tell which view it is
	// looking at without comparing against the empty string.
	Branch *string `json:"branch"`
}

func newKeyListResponse(result keysvc.BrowseResult) keyListResponse {
	out := keyListResponse{
		Keys:    make([]keyResponse, 0, len(result.Keys)),
		Total:   result.Total,
		Limit:   result.Limit,
		Offset:  result.Offset,
		Locales: make([]string, 0, len(result.Locales)),
	}
	for _, l := range result.Locales {
		out.Locales = append(out.Locales, l.Code)
	}
	for _, v := range result.Keys {
		out.Keys = append(out.Keys, newKeyResponse(v, result.Locales))
	}
	if result.Branch != nil {
		name := result.Branch.Name
		out.Branch = &name
	}
	return out
}

// --- request shapes ---------------------------------------------------------

type createKeyRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platforms   []string `json:"platforms"`
	AndroidName *string  `json:"android_name"`
	IOSName     *string  `json:"ios_name"`
}

// patchKeyRequest is a PARTIAL update: an absent field is left alone.
//
// android_name and ios_name use optionalString because they have three states —
// absent, null and a value — and null is a real instruction ("go back to
// deriving it from the name") rather than an absence.
type patchKeyRequest struct {
	Name        *string        `json:"name"`
	Description *string        `json:"description"`
	Platforms   []string       `json:"platforms"`
	AndroidName optionalString `json:"android_name"`
	IOSName     optionalString `json:"ios_name"`
	BaseVersion *int           `json:"base_version"`
}

type putTranslationRequest struct {
	// Value is a pointer so an omitted field is refused rather than silently
	// treated as "". Writing "" is a deliberate act — it means "this string is
	// intentionally blank" — and it must not be what a client gets for
	// forgetting a field. Removing a translation is DELETE, not null.
	Value      *string `json:"value"`
	RenderHint string  `json:"render_hint"`

	// BaseVersion is required on master. See keysvc.SetTranslationRequest.
	BaseVersion *int `json:"base_version"`
}

type deleteTranslationRequest struct {
	BaseVersion *int `json:"base_version"`
}

// conflictResponse is the 409 body for a lost optimistic-concurrency race.
//
// It carries BOTH sides, because that is the entire reason for answering 409
// instead of retrying: a human has to choose, and they cannot choose from a
// message that says only "conflict". Theirs is a cellResponse rather than a
// string so "somebody deleted the translation you were editing" and "somebody
// blanked it" stay different answers.
type conflictResponse struct {
	Error   string `json:"error"`
	Details string `json:"details"`

	KeyID  int64  `json:"key_id"`
	Locale string `json:"locale"`

	// BaseVersion is what the caller sent; Theirs.Version is what it should
	// have been.
	BaseVersion int `json:"base_version"`

	// Mine is what the caller tried to write, and is null for a delete — there
	// is no value in a removal.
	Mine   *string      `json:"mine"`
	Theirs cellResponse `json:"theirs"`
}

// --- handlers ---------------------------------------------------------------

// ListKeys is the key browser's bulk fetch.
//
//	GET /api/v1/keys?branch=copy-fixes&locales=en-SG,ms-MY&platform=flutter
//	                &tag=needs-review&search=top%20up&untranslated_in=th-TH
//
// Viewer. Every parameter is optional and unknown ones are refused: a portal
// that misspells "untranslated_in" must be told, not handed the unfiltered
// corpus and left to wonder why its filter does nothing.
func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch", "locales", "platform", "tag",
		"search", "untranslated_in", "include_deleted", "limit", "offset"); err != nil {
		h.badRequest(w, r, err)
		return
	}

	limit, err := queryInt(r, "limit", 0)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	if limit > maxBrowseLimit {
		h.badRequest(w, r, errors.New("limit must be at most 20000"))
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	includeDeleted, err := queryBool(r, "include_deleted")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	q := r.URL.Query()
	result, err := h.keySvc.Browse(r.Context(), keysvc.BrowseRequest{
		Branch:         q.Get("branch"),
		Locales:        queryList(r, "locales"),
		Platform:       q.Get("platform"),
		Tag:            q.Get("tag"),
		Search:         q.Get("search"),
		UntranslatedIn: q.Get("untranslated_in"),
		IncludeDeleted: includeDeleted,
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		h.portalError(w, r, "list keys", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newKeyListResponse(result))
}

// GetKey returns one key with its values.
//
//	GET /api/v1/keys/{id}?branch=copy-fixes&locales=en-SG
func (h *Handler) GetKey(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch", "locales"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	result, err := h.keySvc.Get(r.Context(), id,
		r.URL.Query().Get("branch"), queryList(r, "locales"))
	if err != nil {
		h.portalError(w, r, "get key", err)
		return
	}
	if len(result.Keys) == 0 {
		// Unreachable — the service returns ErrNotFound — but a zero-length
		// slice indexed at [0] is a panic, and this is a handler.
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, errorResponse{Error: "not_found"})
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newKeyResponse(result.Keys[0], result.Locales))
}

// CreateKey adds a key.
//
//	POST /api/v1/keys
//	{"name":"wallet_top_up_cta","description":"...","platforms":["flutter"]}
//
// Editor. Master only — see keysvc.CreateKey for why a branch cannot yet hold a
// created key.
func (h *Handler) CreateKey(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch"); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body createKeyRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	created, err := h.keySvc.CreateKey(r.Context(), r.URL.Query().Get("branch"),
		keysvc.CreateKeyRequest{
			Name:        body.Name,
			Description: body.Description,
			Platforms:   platformsFrom(body.Platforms),
			AndroidName: body.AndroidName,
			IOSName:     body.IOSName,
		}, identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "create key", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newKeyResponse(keysvc.KeyView{Key: created}, nil))
}

// PatchKey rewrites a key's metadata.
//
//	PATCH /api/v1/keys/{id}?branch=copy-fixes
//	{"description":"shown on the top-up sheet","base_version":3}
//
// Editor. With ?branch= the change is written as a copy-on-write delta and
// master is untouched.
func (h *Handler) PatchKey(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body patchKeyRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	req := keysvc.UpdateKeyRequest{
		KeyID:       id,
		Branch:      r.URL.Query().Get("branch"),
		Name:        body.Name,
		Description: body.Description,
		Platforms:   platformsFrom(body.Platforms),
		BaseVersion: body.BaseVersion,
	}
	// Set-but-null is "clear the override"; set-with-a-value is "use this one";
	// absent is neither.
	if body.AndroidName.Set {
		req.AndroidName = body.AndroidName.Value
		req.ClearAndroidName = body.AndroidName.Value == nil
	}
	if body.IOSName.Set {
		req.IOSName = body.IOSName.Value
		req.ClearIOSName = body.IOSName.Value == nil
	}

	updated, err := h.keySvc.UpdateKey(r.Context(), req,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.portalError(w, r, "update key", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newKeyResponse(keysvc.KeyView{Key: updated}, nil))
}

// DeleteKey soft-deletes a key.
//
//	DELETE /api/v1/keys/{id}?branch=copy-fixes
//
// Editor. The row survives with status 'deleted': history references it, and
// idx_keys_name_active is partial so the name becomes reusable rather than
// reserved forever.
//
// The optional base_version travels in a body rather than a query parameter
// because it is a precondition on the entity, not a selector. A DELETE with a
// body is legal; a DELETE with no body is the common case and is accepted.
func (h *Handler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	baseVersion, err := optionalBaseVersion(w, r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	if _, err := h.keySvc.DeleteKey(r.Context(), id, r.URL.Query().Get("branch"),
		baseVersion, identityActor(r.Context()), middleware.GetReqID(r.Context())); err != nil {
		h.portalError(w, r, "delete key", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// PutTranslation is the inline cell edit.
//
//	PUT /api/v1/keys/{id}/translations/{locale}?branch=copy-fixes
//	{"value":"Top up","base_version":4}
//
// Editor. On master base_version is REQUIRED and a mismatch answers 409 with
// both values in the body, so the portal can show theirs and mine without a
// second round trip. On a branch base_version must be absent: branch edits are
// reconciled against master once, at merge.
func (h *Handler) PutTranslation(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	locale := chi.URLParam(r, "locale")
	if locale == "" {
		h.badRequest(w, r, errors.New("locale is required"))
		return
	}

	var body putTranslationRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}
	if body.Value == nil {
		h.badRequest(w, r, errors.New(
			"value is required; to make a translation untranslated use DELETE, and to make it deliberately blank send an empty string"))
		return
	}

	cell, err := h.keySvc.SetTranslation(r.Context(), keysvc.SetTranslationRequest{
		KeyID:       id,
		LocaleCode:  locale,
		Branch:      r.URL.Query().Get("branch"),
		Value:       *body.Value,
		RenderHint:  model.RenderHint(body.RenderHint),
		BaseVersion: body.BaseVersion,
	}, identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.translationError(w, r, "set translation", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newCellResponse(cell))
}

// DeleteTranslation makes a pair UNTRANSLATED.
//
//	DELETE /api/v1/keys/{id}/translations/{locale}?branch=copy-fixes
//
// Editor. It removes the row; it does NOT write "". An absent row is omitted
// from the export under skip_empty and means "nobody has translated this yet",
// where a row holding "" is a deliberate blank that exports as "". Writing ""
// here would convert ~430 untranslated en-SG keys into intentional blanks.
func (h *Handler) DeleteTranslation(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "branch"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	locale := chi.URLParam(r, "locale")
	if locale == "" {
		h.badRequest(w, r, errors.New("locale is required"))
		return
	}

	baseVersion, err := optionalBaseVersion(w, r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	err = h.keySvc.DeleteTranslation(r.Context(), id, locale,
		r.URL.Query().Get("branch"), baseVersion,
		identityActor(r.Context()), middleware.GetReqID(r.Context()))
	if err != nil {
		h.translationError(w, r, "delete translation", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// KeyHistory returns a key's metadata and value timelines.
//
//	GET /api/v1/keys/{id}/history?locale=en-SG&limit=50
//
// Viewer. Both tables are insert-only: a rollback appears as a new forward
// entry, never as a deletion, because a log that can be edited answers nothing.
func (h *Handler) KeyHistory(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r, "locale", "limit"); err != nil {
		h.badRequest(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		h.badRequest(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", keysvc.DefaultHistoryLimit)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	keyEvents, valueEvents, err := h.keySvc.History(r.Context(), id,
		r.URL.Query().Get("locale"), limit)
	if err != nil {
		h.portalError(w, r, "read key history", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, historyResponse{
		Key:          newKeyHistoryResponses(keyEvents),
		Translations: newTranslationHistoryResponses(valueEvents),
	})
}

type historyResponse struct {
	Key          []keyHistoryResponse         `json:"key"`
	Translations []translationHistoryResponse `json:"translations"`
}

type keyHistoryResponse struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platforms   []string `json:"platforms"`
	Status      string   `json:"status"`
	Version     int      `json:"version"`
	Source      string   `json:"source"`
	// BranchID is null for a change made on master.
	BranchID  *int64 `json:"branch_id"`
	ChangedBy string `json:"changed_by"`
	ChangedAt string `json:"changed_at"`
}

func newKeyHistoryResponses(entries []repository.KeyHistoryEntry) []keyHistoryResponse {
	out := make([]keyHistoryResponse, 0, len(entries))
	for _, e := range entries {
		platforms := make([]string, 0, len(e.Platforms))
		for _, p := range e.Platforms {
			platforms = append(platforms, string(p))
		}
		out = append(out, keyHistoryResponse{
			ID: e.ID, Name: e.Name, Description: e.Description,
			Platforms: platforms, Status: e.Status, Version: e.Version,
			Source: e.Source, BranchID: e.BranchID,
			ChangedBy: e.ChangedBy, ChangedAt: formatTime(e.ChangedAt),
		})
	}
	return out
}

type translationHistoryResponse struct {
	ID     int64  `json:"id"`
	Locale string `json:"locale"`

	// Translated and Value carry the same three states as cellResponse: a NULL
	// value in the history row means the pair BECAME untranslated, which is a
	// different event from it becoming blank.
	Translated bool    `json:"translated"`
	Value      *string `json:"value,omitempty"`

	RenderHint string `json:"render_hint"`
	Version    int    `json:"version"`
	Source     string `json:"source"`
	BranchID   *int64 `json:"branch_id"`
	ChangedBy  string `json:"changed_by"`
	ChangedAt  string `json:"changed_at"`
}

func newTranslationHistoryResponses(entries []repository.TranslationHistoryEntry) []translationHistoryResponse {
	out := make([]translationHistoryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, translationHistoryResponse{
			ID: e.ID, Locale: e.LocaleCode,
			Translated: e.Value != nil, Value: e.Value,
			RenderHint: e.RenderHint, Version: e.Version, Source: e.Source,
			BranchID: e.BranchID, ChangedBy: e.ChangedBy,
			ChangedAt: formatTime(e.ChangedAt),
		})
	}
	return out
}

// --- helpers ----------------------------------------------------------------

// translationError adds the theirs/mine 409 to the shared portal mapping.
//
// A version conflict on a cell is the one refusal the portal cannot render from
// a message alone, so it gets a body carrying both values. Everything else
// falls through to portalError, so the two paths cannot disagree about what a
// 404 or a 400 looks like.
func (h *Handler) translationError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var conflict *keysvc.ConflictError
	if errors.As(err, &conflict) {
		body := conflictResponse{
			Error:       "version_conflict",
			Details:     conflict.Error(),
			KeyID:       conflict.KeyID,
			Locale:      conflict.Locale,
			BaseVersion: conflict.ExpectedVersion,
			Theirs:      newCellResponse(conflict.Theirs),
		}
		if op != "delete translation" {
			mine := conflict.Mine
			body.Mine = &mine
		}

		render.Status(r, http.StatusConflict)
		render.JSON(w, r, body)
		return
	}

	h.portalError(w, r, op, err)
}

// optionalBaseVersion reads a precondition from a DELETE body.
//
// An empty body is the common case and is not an error: most deletes are not
// racing anybody. A malformed body is an error, because a caller who meant to
// send a precondition and got the shape wrong must not have it silently
// dropped — that would turn a guarded delete into an unguarded one.
func optionalBaseVersion(w http.ResponseWriter, r *http.Request) (*int, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, nil
	}
	var body deleteTranslationRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		return nil, err
	}
	return body.BaseVersion, nil
}

func platformsFrom(names []string) []model.Platform {
	if names == nil {
		return nil
	}
	out := make([]model.Platform, 0, len(names))
	for _, n := range names {
		out = append(out, model.Platform(n))
	}
	return out
}
