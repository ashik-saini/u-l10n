package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/branchsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/releasesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/tagsvc"
)

// maxPortalBodyBytes bounds the JSON bodies on the portal routes.
//
// Larger than maxAssetBodyBytes because these bodies carry actual copy — a
// translation value, a key description, a merge-request title — where the asset
// endpoints carry only declarations. Still far below anything that would let a
// caller make the process allocate: the service caps a single value at 20,000
// characters, so this leaves room for a long value plus its envelope and
// nothing more.
const maxPortalBodyBytes = 64 << 10

// timeLayout is how every timestamp leaves this service.
//
// Explicit UTC and an explicit layout rather than time.Time's default marshal,
// so a portal never has to guess at an offset or at sub-second precision that
// varies with the value.
const timeLayout = "2006-01-02T15:04:05Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// identityActor names the human performing a write, for history and audit rows.
//
// The email, not the display name: an audit trail whose actors cannot be
// resolved back to an account answers nothing. The fallback is reachable only if
// one of these routes is ever mounted outside RequireIdentity, and it is a
// legible value rather than "" because audit_events.actor and
// translations.updated_by are both NOT NULL.
func identityActor(ctx context.Context) string {
	if user, ok := IdentityFromContext(ctx); ok {
		return user.Email
	}
	return "unauthenticated"
}

// rejectUnknownParams refuses a query string containing anything not listed.
//
// The same rule parseExportRequest applies, for the same reason: a portal that
// misspells "untranslated_in" must be told, not silently handed an unfiltered
// 6,300-row page and left to wonder why its filter does nothing.
func rejectUnknownParams(r *http.Request, allowed ...string) error {
	permitted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		permitted[name] = true
	}
	for name := range r.URL.Query() {
		if !permitted[name] {
			return fmt.Errorf("unknown query parameter %q (allowed: %s)",
				name, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// queryInt reads a non-negative integer parameter. An absent parameter yields
// the fallback; a present but unparseable one is an error rather than a silent
// fallback, because a caller who sent limit=abc did not mean "use the default".
func queryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	return n, nil
}

// queryBool reads a strict boolean. "yes", "1" and "" are not booleans here:
// accepting a loose vocabulary means accepting "flase" as false.
func queryBool(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	switch raw {
	case "":
		return false, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false, got %q", name, raw)
	}
}

// queryList splits a comma-separated parameter, dropping empty elements so
// "en-SG,,en-MY" and a trailing comma are not errors a human has to hunt for.
func queryList(r *http.Request, name string) []string {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// decodeJSONLimit reads a bounded, strict JSON body.
//
// DisallowUnknownFields for the same reason rejectUnknownParams exists: a
// client that misspells a field must be told, not silently handed a zero value
// and a confusing validation error two layers down.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Exactly one JSON value. Trailing content means the client sent something
	// other than what it thinks it sent.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("body must contain exactly one JSON object")
	}
	return nil
}

func decodePortalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSONLimit(w, r, dst, maxPortalBodyBytes)
}

// optionalString distinguishes THREE states in a PATCH body: the field was
// absent, the field was explicitly null, or the field carries a value.
//
// A plain *string can only express two of them, and for android_name and
// ios_name the third is the one that matters: NULL means "derive from the key
// name", which is a deliberate override being removed rather than any
// particular string being set. The same three-state discipline the translations
// table is built on, applied to the request body.
type optionalString struct {
	// Set reports that the field appeared in the JSON at all.
	Set bool
	// Value is nil when the field appeared as null.
	Value *string
}

func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.Value = &s
	return nil
}

// portalError maps every sentinel the portal services raise onto a status code.
//
// One mapper rather than one per resource, because the mapping must not drift:
// a stale approval answering 409 on one route and 500 on another is a portal
// that cannot tell a refusal from an outage. The rule is the same one asset.go
// follows — everything the caller can act on is a 4xx, and only a genuinely
// unrecognised error reaches 500, where the reason goes to the log and not to
// the body.
func (h *Handler) portalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	// --- 400: the caller sent something wrong -------------------------------
	case errors.Is(err, keysvc.ErrBadRequest),
		errors.Is(err, branchsvc.ErrBadRequest),
		errors.Is(err, mrsvc.ErrBadRequest),
		errors.Is(err, tagsvc.ErrBadRequest),
		errors.Is(err, releasesvc.ErrBadRequest),
		errors.Is(err, projectsvc.ErrBadRequest):
		h.badRequest(w, r, err)

	// --- 404: the thing addressed does not exist ----------------------------
	case errors.Is(err, repository.ErrNotFound):
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, errorResponse{Error: "not_found", Details: err.Error()})

	// --- 409: the caller's view of the world is out of date ------------------
	//
	// Every one of these is an expected outcome a human must act on, not a
	// fault. Answering 500 for any of them would send an on-call engineer after
	// a translator who saved twice.
	case errors.Is(err, repository.ErrOptimisticLock):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "version_conflict", Details: err.Error()})

	case errors.Is(err, repository.ErrKeyNameTaken),
		errors.Is(err, repository.ErrTagNameTaken),
		errors.Is(err, repository.ErrBranchNameTaken):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "name_taken", Details: err.Error()})

	// A project code is a slug, and it appears in URLs — the taken/free split
	// is the caller's mistake to fix, exactly like a name collision above.
	case errors.Is(err, repository.ErrProjectCodeTaken):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "project_code_taken", Details: err.Error()})

	// A locale code is likewise the caller's mistake to fix: the project
	// already has one.
	case errors.Is(err, repository.ErrLocaleCodeTaken):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "locale_code_taken", Details: err.Error()})

	// Left to the database's UNIQUE (project_id, flutter_dir|android_values_dir|ios_lproj)
	// rather than pre-checked — see LocaleRepository.Create's doc comment for why.
	case errors.Is(err, repository.ErrLocaleDirectoryTaken):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "locale_directory_taken", Details: err.Error()})

	case errors.Is(err, keysvc.ErrBranchNotOpen),
		errors.Is(err, branchsvc.ErrBranchNotOpen):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "branch_not_open", Details: err.Error()})

	case errors.Is(err, mrsvc.ErrNotLive):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "merge_request_not_live", Details: err.Error()})

	case errors.Is(err, repository.ErrLiveMergeRequestExists):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "live_merge_request_exists", Details: err.Error()})

	// A guarded status transition that matched no row: the request left the
	// expected state under the caller. mrsvc wraps this into ErrNotLive on its
	// own paths; this entry is the backstop for the merge transaction's final
	// approved → merged move, so a lost race stays a 409 and never a 500.
	case errors.Is(err, repository.ErrStaleMergeRequestStatus):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "merge_request_not_live", Details: err.Error()})

	// Two simultaneous publishes picked the same version number and the unique
	// index refused the loser. Retryable by design — the comment on
	// CreatePublish promises exactly this outcome.
	case errors.Is(err, repository.ErrReleaseVersionRace):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "release_version_race", Details: err.Error()})

	// A master write raced the merge between its conflict computation and its
	// apply. The transaction rolled back whole; retrying the merge surfaces
	// the new conflict for a human to resolve.
	case errors.Is(err, mergesvc.ErrConcurrentMasterWrite):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "concurrent_master_write", Details: err.Error()})

	// The four merge refusals. Every one is an expected outcome a human must
	// act on, and answering 500 for any of them would page an engineer because
	// two translators edited the same string. MergeMergeRequest answers the
	// first two itself, with the offending rows in the body; these entries make
	// sure no other path can turn them into a 500.
	case errors.Is(err, mergesvc.ErrStaleApproval):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "stale_approval", Details: err.Error()})

	case errors.Is(err, mergesvc.ErrNotApproved):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "not_approved", Details: err.Error()})

	case errors.Is(err, mergesvc.ErrUnresolvedConflicts):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "unresolved_conflicts", Details: err.Error()})

	case errors.Is(err, repository.ErrAlreadyRolledBack):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "already_rolled_back", Details: err.Error()})

	case errors.Is(err, mergesvc.ErrNameCollision):
		render.Status(r, http.StatusConflict)
		render.JSON(w, r, errorResponse{Error: "name_collision", Details: err.Error()})

	default:
		log.Errore(r.Context(), op+" failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "internal_error"})
	}
}
