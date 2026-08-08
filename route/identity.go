package route

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/googleauth"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TokenVerifier is the narrowest view of pkg/googleauth this package needs:
// "whose token is this?".
//
// Declared here, and satisfied by *googleauth.Verifier, for the same reason
// route.Pinger and assetsvc.ObjectStore exist — the real implementation talks
// to accounts.google.com, and a middleware whose refusal paths can only be
// exercised with a live Google token is a middleware whose refusal paths are
// never exercised.
type TokenVerifier interface {
	Verify(ctx context.Context, accessToken string) (googleauth.TokenInfo, error)
}

// ctxKeyIdentity carries the authenticated operator through the request context.
//
// An unexported struct type as the key, so no other package can collide with it
// or forge an entry. Distinct from ctxKeyToken: a script and a human are
// different principals and code downstream must not be able to confuse them.
type ctxKeyIdentity struct{}

// IdentityFromContext returns the authenticated operator, if any.
func IdentityFromContext(ctx context.Context) (repository.User, bool) {
	u, ok := ctx.Value(ctxKeyIdentity{}).(repository.User)
	return u, ok
}

// RequireIdentity authenticates a human operator via the portal's Google access
// token, and enforces a minimum role.
//
// The counterpart to RequireAPIToken: same structure, same error shapes, a
// different kind of principal. Two facts are established in order, and they are
// separate on purpose:
//
//  1. WHO — the opaque bearer token is validated against Google, yielding an
//     email. Failure here is 401: re-authenticating might help.
//  2. WHAT — that email is looked up in this service's own users table. Failure
//     here is 403: re-authenticating will never help, and telling someone
//     without an account to sign in again sends them round that loop forever.
//
// The `x-yp-role` header the portal also sends is ignored entirely. It
// describes YouPortal's yp_* group model, which says nothing about localization
// — a designer may be an l10n editor and nothing else — and it arrives from the
// client, which makes it a request, not a fact. The portal must gate its UI on
// GET /api/v1/me.
func (h *Handler) RequireIdentity(minRole string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			presented := bearerToken(r)
			if presented == "" {
				challenge(w, r, "missing_token")
				return
			}

			info, err := h.identities.Verify(ctx, presented)
			if err != nil {
				if errors.Is(err, googleauth.ErrInvalidToken) {
					// Expired, revoked, fabricated, or issued to another OAuth
					// client — one response for all of them. The prefix is
					// logged, never the token.
					log.Infow(ctx, "rejected access token",
						"reason", "invalid", "prefix", safeTokenPrefix(presented))
					challenge(w, r, "invalid_token")
					return
				}
				// Google unreachable or answering 5xx. NOT a 401: the token may
				// be perfectly good, and answering 401 during someone else's
				// outage tells every operator in the building to sign in again.
				// And NOT a 500: the fault is a dependency, not this service,
				// so 503 is the honest status — and the retryable one, letting
				// the portal back off and retry rather than treating it as a
				// server bug.
				log.Errore(ctx, "access token verification failed", err)
				w.Header().Set("Retry-After", "5")
				render.Status(r, http.StatusServiceUnavailable)
				render.JSON(w, r, errorResponse{Error: "identity_provider_unavailable"})
				return
			}

			user, err := h.users.ByEmail(ctx, nil, info.Email)
			if err != nil {
				if errors.Is(err, repository.ErrNotFound) {
					// Authenticated, but has no account here. 403, not 401.
					log.Infow(ctx, "rejected identity: no user record", "email", info.Email)
					forbidden(w, r, "not_provisioned",
						"this account has no access to u-l10n")
					return
				}
				log.Errore(ctx, "user lookup failed", err)
				render.Status(r, http.StatusInternalServerError)
				render.JSON(w, r, errorResponse{Error: "internal_error"})
				return
			}

			// Disabled is a revocation, and it must outrank whatever role the
			// row still carries — an offboarded admin is still an admin in the
			// role column.
			if user.Status != repository.StatusActive {
				log.Infow(ctx, "rejected identity: account disabled",
					"email", user.Email, "status", user.Status)
				forbidden(w, r, "account_disabled", "this account is disabled")
				return
			}

			if !roleSatisfies(user.Role, minRole) {
				log.Infow(ctx, "identity role insufficient",
					"email", user.Email, "has", user.Role, "needs", minRole)
				forbidden(w, r, "insufficient_role",
					"this account cannot perform this action")
				return
			}

			next.ServeHTTP(w, r.WithContext(
				context.WithValue(ctx, ctxKeyIdentity{}, user)))
		})
	}
}

// roleRank orders the roles.
//
// Unknown roles rank -1 — below viewer — so a value that somehow bypassed the
// users_role_check constraint, or one added to the database ahead of the code
// that understands it, fails closed rather than being treated as privileged.
// roleSatisfies rejects anything non-positive on either side, so an unknown
// role satisfies nothing at all, not even a viewer minimum.
func roleRank(role string) int {
	switch role {
	case repository.RoleViewer:
		return 1
	case repository.RoleEditor:
		return 2
	case repository.RoleApprover:
		return 3
	case repository.RoleAdmin:
		return 4
	default:
		return -1
	}
}

func roleSatisfies(have, need string) bool {
	h, n := roleRank(have), roleRank(need)
	return h > 0 && n > 0 && h >= n
}

// bearerToken extracts the credential from the Authorization header.
//
// The scheme match is case-insensitive because RFC 7235 says it is; the value
// is not trimmed of anything but surrounding whitespace, since a token is
// opaque and "cleaning" it can only turn a valid credential into an invalid
// one.
func bearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const scheme = "bearer "
	if len(auth) < len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(auth[len(scheme):])
}

// safeTokenPrefix returns just enough of a presented token to correlate log
// lines, and never enough to use.
//
// Shorter than safePrefix's window because a Google access token has no
// recognisable prefix to skip past — every character shown is a character of
// the secret.
func safeTokenPrefix(presented string) string {
	const show = 6
	if len(presented) <= show {
		return "(too short)"
	}
	return presented[:show] + "..."
}

// challenge answers 401 with a WWW-Authenticate header, so a client sees which
// credential was expected rather than guessing at a bare 401.
func challenge(w http.ResponseWriter, r *http.Request, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="u-l10n"`)
	render.Status(r, http.StatusUnauthorized)
	render.JSON(w, r, errorResponse{
		Error:   reason,
		Details: "provide a Google access token in the Authorization header",
	})
}

// forbidden answers 403 and deliberately carries NO WWW-Authenticate header:
// the caller proved who they are, and inviting them to try again would be a lie.
func forbidden(w http.ResponseWriter, r *http.Request, reason, details string) {
	render.Status(r, http.StatusForbidden)
	render.JSON(w, r, errorResponse{Error: reason, Details: details})
}
