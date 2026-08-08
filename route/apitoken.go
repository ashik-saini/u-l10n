package route

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// ctxKeyToken carries the authenticated token through the request context.
//
// An unexported struct type as the key, so no other package can collide with it
// or forge an entry.
type ctxKeyToken struct{}

// TokenFromContext returns the authenticated token, if any.
func TokenFromContext(ctx context.Context) (repository.APIToken, bool) {
	t, ok := ctx.Value(ctxKeyToken{}).(repository.APIToken)
	return t, ok
}

// RequireAPIToken authenticates scripts and CI via the X-Api-Token header, and
// enforces a minimum scope.
//
// Scopes are ordered, so requiring read_export also admits read_write. Ordering
// lets a handler state the minimum it needs rather than enumerating every scope
// that qualifies — the same reasoning as the ordered user roles.
func (h *Handler) RequireAPIToken(minScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := strings.TrimSpace(r.Header.Get("X-Api-Token"))
			if presented == "" {
				// WWW-Authenticate so a script author sees what was expected
				// rather than guessing at a bare 401.
				w.Header().Set("WWW-Authenticate", `X-Api-Token realm="u-l10n"`)
				unauthorized(w, r, "missing_token")
				return
			}

			token, err := h.tokens.Authenticate(r.Context(), nil, presented)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					// One response for wrong, revoked and expired alike: a
					// distinguishable error tells an attacker which tokens
					// exist. The prefix is logged, never the token.
					log.Infow(r.Context(), "rejected api token",
						"reason", "not_live", "prefix", safePrefix(presented))
					unauthorized(w, r, "invalid_token")
					return
				}
				log.Errore(r.Context(), "api token lookup failed", err)
				render.Status(r, http.StatusInternalServerError)
				render.JSON(w, r, errorResponse{Error: "internal_error"})
				return
			}

			if !scopeSatisfies(token.Scope, minScope) {
				// 403, not 401: the caller is authenticated, just not permitted.
				// Conflating the two sends them to re-authenticate forever.
				log.Infow(r.Context(), "api token scope insufficient",
					"token", token.Name, "has", token.Scope, "needs", minScope)
				render.Status(r, http.StatusForbidden)
				render.JSON(w, r, errorResponse{
					Error:   "insufficient_scope",
					Details: "this token cannot perform this action",
				})
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyToken{}, token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// scopeRank orders the scopes. Unknown scopes rank -1 so a value that somehow
// bypassed the CHECK constraint fails closed rather than being treated as
// privileged.
func scopeRank(scope string) int {
	switch scope {
	case repository.ScopeReadExport:
		return 1
	case repository.ScopeReadWrite:
		return 2
	default:
		return -1
	}
}

func scopeSatisfies(have, need string) bool {
	h, n := scopeRank(have), scopeRank(need)
	return h > 0 && n > 0 && h >= n
}

// safePrefix returns just enough of a presented token to correlate log lines,
// and never enough to use. Tokens must not reach logs intact.
func safePrefix(presented string) string {
	const show = len(repository.TokenPrefix) + 6
	if len(presented) <= show {
		return "(too short)"
	}
	return presented[:show] + "..."
}

func unauthorized(w http.ResponseWriter, r *http.Request, reason string) {
	render.Status(r, http.StatusUnauthorized)
	render.JSON(w, r, errorResponse{
		Error:   reason,
		Details: "provide a valid token in the X-Api-Token header",
	})
}
