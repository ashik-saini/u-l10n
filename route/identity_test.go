package route

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/apm"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/googleauth"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// --- doubles ----------------------------------------------------------------

// stubVerifier stands in for Google. The real one talks to accounts.google.com,
// which is exactly why route depends on the TokenVerifier interface.
type stubVerifier struct {
	info  googleauth.TokenInfo
	err   error
	calls int
	// seen records what was presented, so a test can prove the middleware
	// forwards the token it was given and nothing else.
	seen []string
}

func (s *stubVerifier) Verify(_ context.Context, token string) (googleauth.TokenInfo, error) {
	s.calls++
	s.seen = append(s.seen, token)
	if s.err != nil {
		return googleauth.TokenInfo{}, s.err
	}
	return s.info, nil
}

// stubUsers is a UserRepository backed by a map.
type stubUsers struct {
	byEmail map[string]repository.User
	err     error
	calls   int
}

func (s *stubUsers) ByEmail(_ context.Context, _ *gorm.DB, email string) (repository.User, error) {
	s.calls++
	if s.err != nil {
		return repository.User{}, s.err
	}
	// The real column is CITEXT, so the lookup is case-insensitive by type.
	for k, u := range s.byEmail {
		if strings.EqualFold(k, email) {
			return u, nil
		}
	}
	return repository.User{}, fmt.Errorf("user %q: %w", email, repository.ErrNotFound)
}

func (s *stubUsers) Upsert(context.Context, *gorm.DB, repository.User) (repository.User, error) {
	return repository.User{}, nil
}

func (s *stubUsers) List(context.Context, *gorm.DB) ([]repository.User, error) {
	return nil, nil
}

func (s *stubUsers) SetRole(context.Context, *gorm.DB, string, string) (repository.User, error) {
	return repository.User{}, nil
}

// identityHandler builds a Handler with the two doubles wired in.
func identityHandler(v *stubVerifier, u *stubUsers) *Handler {
	return &Handler{
		cnf:        &config.Config{RequestTimeout: time.Second},
		identities: v,
		users:      u,
	}
}

// runMiddleware drives RequireIdentity and reports what reached the far side.
func runMiddleware(h *Handler, minRole, authHeader string) (*httptest.ResponseRecorder, *repository.User) {
	var reached *repository.User

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := IdentityFromContext(r.Context()); ok {
			reached = &u
		}
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	// The portal sends this on every request. It describes YouPortal's group
	// model and must have no effect here whatsoever.
	r.Header.Set("x-yp-role", "yp_sg_admin")

	h.RequireIdentity(minRole)(next).ServeHTTP(rec, r)
	return rec, reached
}

func user(email, role, status string) repository.User {
	return repository.User{ID: 1, Email: email, Role: role, Status: status}
}

// --- the refusals -----------------------------------------------------------

// TestRequireIdentityRefusals is the test that matters. A middleware that
// admits someone it should refuse is the bug this file exists to catch, and the
// 401/403 split is the one most often got wrong: telling a person with no
// account to re-authenticate sends them round that loop forever.
func TestRequireIdentityRefusals(t *testing.T) {
	const goodToken = "ya29.valid-access-token"

	cases := []struct {
		name       string
		minRole    string
		authHeader string
		verifier   *stubVerifier
		users      map[string]repository.User
		usersErr   error
		wantStatus int
		wantError  string
		// wantChallenge asserts the presence of WWW-Authenticate. It must appear
		// on 401 and never on 403: a 403 that invites re-authentication is a lie.
		wantChallenge bool
		// wantRetryAfter asserts the Retry-After header. Only the 503 answers
		// carry it: they are the one refusal where trying again later helps.
		wantRetryAfter string
	}{
		{
			name: "no header at all", minRole: repository.RoleViewer,
			verifier:   &stubVerifier{},
			wantStatus: http.StatusUnauthorized, wantError: "missing_token", wantChallenge: true,
		},
		{
			name: "wrong scheme", minRole: repository.RoleViewer,
			authHeader: "Basic dXNlcjpwYXNz", verifier: &stubVerifier{},
			wantStatus: http.StatusUnauthorized, wantError: "missing_token", wantChallenge: true,
		},
		{
			name: "bearer with nothing after it", minRole: repository.RoleViewer,
			authHeader: "Bearer   ", verifier: &stubVerifier{},
			wantStatus: http.StatusUnauthorized, wantError: "missing_token", wantChallenge: true,
		},
		{
			// The X-Api-Token credential is a different principal entirely, and
			// presenting one here must not authenticate anybody.
			name: "an api token is not an identity", minRole: repository.RoleViewer,
			authHeader: "Bearer ul10n_abcdef",
			verifier:   &stubVerifier{err: googleauth.ErrInvalidToken},
			wantStatus: http.StatusUnauthorized, wantError: "invalid_token", wantChallenge: true,
		},
		{
			name: "expired or garbage token", minRole: repository.RoleViewer,
			authHeader: "Bearer expired",
			verifier:   &stubVerifier{err: googleauth.ErrInvalidToken},
			wantStatus: http.StatusUnauthorized, wantError: "invalid_token", wantChallenge: true,
		},
		{
			// Google unreachable. NOT a 401 — the token may be perfectly good,
			// and 401 would send the whole building to re-authenticate during
			// someone else's outage. And NOT a 500 — the fault is a dependency,
			// not this service, so it is 503 with a Retry-After the portal can
			// back off on.
			name: "identity provider is down", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{err: errors.New("dial tcp: i/o timeout")},
			wantStatus: http.StatusServiceUnavailable, wantError: "identity_provider_unavailable",
			wantRetryAfter: "5",
		},
		{
			// THE case. Valid Google token, no row in users.
			name: "authenticated but not provisioned", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "stranger@you.co"}},
			users:      map[string]repository.User{},
			wantStatus: http.StatusForbidden, wantError: "not_provisioned",
		},
		{
			// Disabled outranks the role column: an offboarded admin is still
			// 'admin' in that column.
			name: "disabled admin", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "gone@you.co"}},
			users: map[string]repository.User{
				"gone@you.co": user("gone@you.co", repository.RoleAdmin, repository.StatusDisabled),
			},
			wantStatus: http.StatusForbidden, wantError: "account_disabled",
		},
		{
			name: "viewer reaching for an editor endpoint", minRole: repository.RoleEditor,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "v@you.co"}},
			users: map[string]repository.User{
				"v@you.co": user("v@you.co", repository.RoleViewer, repository.StatusActive),
			},
			wantStatus: http.StatusForbidden, wantError: "insufficient_role",
		},
		{
			name: "approver reaching for an admin endpoint", minRole: repository.RoleAdmin,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "a@you.co"}},
			users: map[string]repository.User{
				"a@you.co": user("a@you.co", repository.RoleApprover, repository.StatusActive),
			},
			wantStatus: http.StatusForbidden, wantError: "insufficient_role",
		},
		{
			// A role the code does not understand must rank BELOW viewer, not
			// above it. This is the fail-closed case: a value added to the
			// database ahead of the code that understands it, or one that
			// somehow bypassed users_role_check, must grant nothing.
			name: "unrecognised role grants nothing", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "x@you.co"}},
			users: map[string]repository.User{
				"x@you.co": user("x@you.co", "superadmin", repository.StatusActive),
			},
			wantStatus: http.StatusForbidden, wantError: "insufficient_role",
		},
		{
			// An empty role column is not a viewer.
			name: "empty role grants nothing", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "x@you.co"}},
			users: map[string]repository.User{
				"x@you.co": user("x@you.co", "", repository.StatusActive),
			},
			wantStatus: http.StatusForbidden, wantError: "insufficient_role",
		},
		{
			// The database is unreachable. Not the caller's fault and not a
			// refusal of their credentials.
			name: "user lookup fails", minRole: repository.RoleViewer,
			authHeader: "Bearer " + goodToken,
			verifier:   &stubVerifier{info: googleauth.TokenInfo{Email: "a@you.co"}},
			usersErr:   errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError, wantError: "internal_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users := &stubUsers{byEmail: tc.users, err: tc.usersErr}
			h := identityHandler(tc.verifier, users)

			rec, reached := runMiddleware(h, tc.minRole, tc.authHeader)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantError)
			assert.Nil(t, reached, "a refused request must never reach the handler")

			challenge := rec.Header().Get("WWW-Authenticate")
			if tc.wantChallenge {
				assert.Equal(t, `Bearer realm="u-l10n"`, challenge)
			} else {
				assert.Empty(t, challenge,
					"only a 401 may invite the caller to authenticate again")
			}

			assert.Equal(t, tc.wantRetryAfter, rec.Header().Get("Retry-After"),
				"Retry-After belongs on 503 and nowhere else")
		})
	}
}

// TestRequireIdentityAdmitsAndCarriesTheIdentity.
func TestRequireIdentityAdmitsAndCarriesTheIdentity(t *testing.T) {
	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: "Ashik.Saini@you.co", ExpiresIn: 3600}}
	users := &stubUsers{byEmail: map[string]repository.User{
		// Stored lowercase; presented mixed-case. users.email is CITEXT, so
		// these are the same person by type rather than by every call site
		// remembering LOWER().
		"ashik.saini@you.co": user("ashik.saini@you.co", repository.RoleApprover, repository.StatusActive),
	}}

	rec, reached := runMiddleware(identityHandler(verifier, users),
		repository.RoleEditor, "Bearer ya29.good")

	assert.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, reached, "an admitted request must carry its identity")
	assert.Equal(t, "ashik.saini@you.co", reached.Email)
	assert.Equal(t, repository.RoleApprover, reached.Role)

	// The scheme is stripped and nothing else is: a token is opaque, and
	// "cleaning" one can only turn a valid credential into an invalid one.
	assert.Equal(t, []string{"ya29.good"}, verifier.seen)
}

// TestRequireIdentityIgnoresTheYPRoleHeader.
//
// The portal sends x-yp-role on every request. Honouring it would mean this
// service's authorization is decided by a header the client controls, and would
// tie u-l10n's roles to YouPortal's yp_* group model — which says nothing about
// localization, since a designer may be an l10n editor and nothing else.
func TestRequireIdentityIgnoresTheYPRoleHeader(t *testing.T) {
	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: "v@you.co"}}
	users := &stubUsers{byEmail: map[string]repository.User{
		"v@you.co": user("v@you.co", repository.RoleViewer, repository.StatusActive),
	}}
	h := identityHandler(verifier, users)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/x@you.co/role", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	r.Header.Set("x-yp-role", "yp_sg_admin")

	h.RequireIdentity(repository.RoleAdmin)(next).ServeHTTP(rec, r)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a viewer claiming yp_sg_admin is still a viewer")
}

// TestRequireIdentityNeverLeaksTheToken. The presented credential must not
// reach the response body; the log gets a short prefix at most.
func TestRequireIdentityNeverLeaksTheToken(t *testing.T) {
	const secret = "ya29.a0AfB_super-secret-google-access-token"

	rec, _ := runMiddleware(
		identityHandler(&stubVerifier{err: googleauth.ErrInvalidToken}, &stubUsers{}),
		repository.RoleViewer, "Bearer "+secret)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)
}

func TestSafeTokenPrefix(t *testing.T) {
	assert.Equal(t, "(too short)", safeTokenPrefix("short"))
	assert.Equal(t, "ya29.a...", safeTokenPrefix("ya29.a0AfB_the-rest-is-secret"))
	// Never the whole thing, however long the window.
	assert.NotContains(t, safeTokenPrefix("ya29.a0AfB_the-rest-is-secret"), "rest-is-secret")
}

// TestRoleOrdering pins the ladder. Ordering is what lets a handler state the
// minimum it needs rather than enumerating every role that qualifies.
func TestRoleOrdering(t *testing.T) {
	assert.Less(t, roleRank(repository.RoleViewer), roleRank(repository.RoleEditor))
	assert.Less(t, roleRank(repository.RoleEditor), roleRank(repository.RoleApprover))
	assert.Less(t, roleRank(repository.RoleApprover), roleRank(repository.RoleAdmin))

	// Unknown ranks below viewer, so an unexpected value fails closed.
	assert.Less(t, roleRank("superadmin"), roleRank(repository.RoleViewer))
	assert.Less(t, roleRank(""), roleRank(repository.RoleViewer))
	assert.Less(t, roleRank("ADMIN"), roleRank(repository.RoleViewer))

	cases := []struct {
		have, need string
		want       bool
	}{
		{repository.RoleAdmin, repository.RoleViewer, true},
		{repository.RoleAdmin, repository.RoleAdmin, true},
		{repository.RoleApprover, repository.RoleEditor, true},
		{repository.RoleEditor, repository.RoleEditor, true},
		{repository.RoleViewer, repository.RoleViewer, true},
		{repository.RoleViewer, repository.RoleEditor, false},
		{repository.RoleEditor, repository.RoleApprover, false},
		{repository.RoleApprover, repository.RoleAdmin, false},

		// An unknown role satisfies nothing, not even the lowest minimum.
		{"superadmin", repository.RoleViewer, false},
		{"", repository.RoleViewer, false},
		{"Admin", repository.RoleViewer, false},

		// An unknown MINIMUM is satisfied by nothing either. A handler that
		// asks for a role that does not exist — a typo in a route definition —
		// must refuse everybody rather than admit everybody.
		{repository.RoleAdmin, "superuser", false},
		{repository.RoleAdmin, "", false},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q satisfies %q", tc.have, tc.need), func(t *testing.T) {
			assert.Equal(t, tc.want, roleSatisfies(tc.have, tc.need))
		})
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct{ header, want string }{
		{"Bearer abc", "abc"},
		// RFC 7235 says the scheme is case-insensitive.
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"  Bearer   abc  ", "abc"},
		{"Basic abc", ""},
		{"abc", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"", ""},
		// A token containing spaces is not one we can recover unambiguously,
		// but the trailing part is opaque and must survive intact.
		{"Bearer a.b-c_d", "a.b-c_d"},
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			assert.Equal(t, tc.want, bearerToken(r))
		})
	}
}

// --- the routes -------------------------------------------------------------

// TestPortalRoutesRequireAnIdentity walks the real router.
//
// A route accidentally mounted outside RequireIdentity would be invisible in a
// unit test of the handler itself, which is why this drives ProvideRoutes. The
// user service is deliberately nil: if a request reached the PATCH handler the
// test would panic rather than quietly pass.
func TestPortalRoutesRequireAnIdentity(t *testing.T) {
	verifier := &stubVerifier{err: googleauth.ErrInvalidToken}
	handler := identityHandler(verifier, &stubUsers{})
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me"},
		{http.MethodPatch, "/api/v1/admin/users/a@you.co/role"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "no token at all")

			rec = httptest.NewRecorder()
			r := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
			r.Header.Set("Authorization", "Bearer bogus")
			// An API token must not open a portal route either.
			r.Header.Set("X-Api-Token", "ul10n_abcdef")
			router.ServeHTTP(rec, r)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "unknown token")
		})
	}

	// One call per route, not two: a request with no Authorization header is
	// refused before Google is asked. Spending a round trip on a client bug is
	// how a login page refresh loop becomes a Google quota incident.
	assert.Equal(t, len(routes), verifier.calls,
		"every portal route must reach the identity check")
}

// TestAdminRouteRequiresAdmin: the role gate is per-route, and /me must not be
// the same gate as the role-granting endpoint.
func TestAdminRouteRequiresAdmin(t *testing.T) {
	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: "e@you.co"}}
	users := &stubUsers{byEmail: map[string]repository.User{
		"e@you.co": user("e@you.co", repository.RoleEditor, repository.StatusActive),
	}}
	handler := identityHandler(verifier, users)
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/x@you.co/role",
		strings.NewReader(`{"role":"admin"}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an editor must not be able to promote themselves")
	assert.Contains(t, rec.Body.String(), "insufficient_role")
}

// TestMeReportsTheRoleThePortalShouldGateOn.
func TestMeReportsTheRoleThePortalShouldGateOn(t *testing.T) {
	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: "a@you.co"}}
	users := &stubUsers{byEmail: map[string]repository.User{
		"a@you.co": user("a@you.co", repository.RoleApprover, repository.StatusActive),
	}}
	handler := identityHandler(verifier, users)
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	// Claiming an admin role in the header changes nothing.
	r.Header.Set("x-yp-role", "yp_sg_admin")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"email":"a@you.co","role":"approver"}`, rec.Body.String())
}

// TestMeCostsNoExtraQuery: RequireIdentity has already read the row, and asking
// the database twice on the busiest endpoint in the service is a self-inflicted
// cost.
func TestMeCostsNoExtraQuery(t *testing.T) {
	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: "a@you.co"}}
	users := &stubUsers{byEmail: map[string]repository.User{
		"a@you.co": user("a@you.co", repository.RoleViewer, repository.StatusActive),
	}}
	handler := identityHandler(verifier, users)
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(httptest.NewRecorder(), r)

	assert.Equal(t, 1, users.calls)
}

// TestMeWithoutAnIdentityDoesNotPanic guards the fallback in Me: if the route
// is ever remounted outside RequireIdentity it must answer 401 rather than
// serve a zero-value user with an empty email and an empty role.
func TestMeWithoutAnIdentityDoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).Me(rec, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"role":""`)
}
