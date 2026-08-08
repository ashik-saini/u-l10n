package route

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
)

type identityResponse struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// Me returns the caller's identity and role.
//
//	GET /api/v1/me  ->  {"email":"a@you.co","role":"editor"}
//
// This is what the portal gates its UI on — NOT the x-yp-role header it sends
// us, which describes YouPortal's own group model and is ignored here.
//
// It requires only the viewer minimum, so every provisioned operator can ask.
// Nothing is queried: RequireIdentity has already read the row, and asking the
// database twice for the same answer on the busiest endpoint in the service
// would be a self-inflicted cost.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	user, ok := IdentityFromContext(r.Context())
	if !ok {
		// Only reachable if this route is ever mounted outside RequireIdentity.
		// It answers 401 rather than panicking on a zero value, and the test in
		// identity_test.go exists to make sure it stays mounted.
		challenge(w, r, "missing_token")
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, identityResponse{Email: user.Email, Role: user.Role})
}

// SetUserRole changes an operator's role.
//
//	PATCH /api/v1/admin/users/{email}/role
//	{"role":"approver"}
//
// Admin only, and audited: who may edit and approve customer-facing copy is
// exactly the kind of fact that has to be reconstructable months later.
//
// It changes an existing user and never creates one — a typo'd address is a 404
// rather than a new account for a person who does not exist. Creating the first
// user is the `user grant` CLI command's job; see main.go for why that cannot
// be an API call.
func (h *Handler) SetUserRole(w http.ResponseWriter, r *http.Request) {
	email, err := pathEmail(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body setRoleRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	actor, _ := IdentityFromContext(r.Context())

	updated, err := h.userSvc.SetRole(r.Context(), email, body.Role,
		actor.Email, middleware.GetReqID(r.Context()))
	if err != nil {
		h.userError(w, r, "set role", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, identityResponse{Email: updated.Email, Role: updated.Role})
}

// userError maps the service's sentinels onto status codes. Everything the
// caller can fix is a 4xx; only an unrecognised error reaches 500.
func (h *Handler) userError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, usersvc.ErrBadRequest):
		h.badRequest(w, r, err)

	case errors.Is(err, repository.ErrNotFound):
		render.Status(r, http.StatusNotFound)
		render.JSON(w, r, errorResponse{Error: "not_found", Details: err.Error()})

	default:
		log.Errore(r.Context(), "user "+op+" failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "internal_error"})
	}
}

// pathEmail reads the {email} path parameter.
//
// chi hands back the raw path segment, so an address containing a character the
// client percent-encoded arrives encoded. Decoding failure is the caller's
// problem and is reported as one rather than being passed through to become a
// confusing "no such user".
func pathEmail(r *http.Request) (string, error) {
	raw := chi.URLParam(r, "email")
	if raw == "" {
		return "", errors.New("email is required")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", errors.New("email is not a valid path segment")
	}
	return decoded, nil
}
