package route

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
)

// TestProjectErrorStatusCodes pins the sentinel-to-status mapping. A
// duplicate code is the caller's mistake and must not reach 500.
func TestProjectErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"bad code", fmt.Errorf("%w: code must be", projectsvc.ErrBadRequest), http.StatusBadRequest, "bad_request"},
		{"duplicate code", fmt.Errorf("create: %w", repository.ErrProjectCodeTaken), http.StatusConflict, "project_code_taken"},
		{"no such project", fmt.Errorf("project %q: %w", "nope", repository.ErrNotFound), http.StatusNotFound, "not_found"},
		{"duplicate locale code", fmt.Errorf("create locale: %w", repository.ErrLocaleCodeTaken), http.StatusConflict, "locale_code_taken"},
		{"duplicate locale directory", fmt.Errorf("create locale: %w", repository.ErrLocaleDirectoryTaken), http.StatusConflict, "locale_directory_taken"},
		{"database down", errors.New("dial tcp: connection refused"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/projects", nil)

			(&Handler{}).projectError(w, r, "create project", tc.err)

			assert.Equal(t, tc.want, w.Code)
			assert.Contains(t, w.Body.String(), tc.code)
		})
	}
}

// TestCreateProjectRequiresPlatformAdmin: minting a project is the one
// global privilege. An admin on YouTrip is still only an admin on YouTrip,
// and must not be able to create YouBiz.
//
// The project service is nil, so a request that got past the middleware
// would panic rather than quietly pass.
func TestCreateProjectRequiresPlatformAdmin(t *testing.T) {
	router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, false)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects",
		strings.NewReader(`{"code":"youbiz","name":"YouBiz"}`))
	r.Header.Set("Content-Type", "application/json")
	// The stub authenticator answers whoever presents a bearer token, so the
	// token needs to be here for the request to reach requirePlatformAdmin at
	// all — otherwise RequireIdentity would refuse it at 401 before the
	// privilege boundary this test exists to check is ever exercised.
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "forbidden")
}

// TestProjectRoutesRejectUnknownQueryParameters: these routes take no query
// parameters, and a stray one is a caller mistake to report, not ignore.
func TestProjectRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, true)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects?typo=1",
		strings.NewReader(`{"code":"youbiz","name":"YouBiz"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "typo")
}

// TestLocaleRoutesRequirePlatformAdmin: adding or reconfiguring a project's
// locale dimension sits behind the same global privilege as minting the
// project itself. Both handlers are nil on this Handler, so a request that
// got past the middleware would panic rather than quietly passing — the
// panic never happens here because the test asserts the refusal instead.
func TestLocaleRoutesRequirePlatformAdmin(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"add locale", http.MethodPost, "/api/v1/projects/youtrip/locales",
			`{"code":"vi-VN","flutter_dir":"vi_VN","android_values_dir":"values-vi","ios_lproj":"vi-VN.lproj"}`},
		{"patch locale", http.MethodPatch, "/api/v1/projects/youtrip/locales/en-SG",
			`{"flutter_dir":"en_SG","android_values_dir":"values","ios_lproj":"en-SG.lproj","sort_order":1,"status":"active"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, false)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			// See TestCreateProjectRequiresPlatformAdmin: the token has to be
			// present for the request to reach requirePlatformAdmin at all.
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(w, r)

			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Contains(t, w.Body.String(), "forbidden")
		})
	}
}

// TestLocaleRoutesRejectUnknownQueryParameters mirrors
// TestProjectRoutesRejectUnknownQueryParameters for the two locale routes.
func TestLocaleRoutesRejectUnknownQueryParameters(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"add locale", http.MethodPost, "/api/v1/projects/youtrip/locales?typo=1",
			`{"code":"vi-VN","flutter_dir":"vi_VN","android_values_dir":"values-vi","ios_lproj":"vi-VN.lproj"}`},
		{"patch locale", http.MethodPatch, "/api/v1/projects/youtrip/locales/en-SG?typo=1",
			`{"flutter_dir":"en_SG","android_values_dir":"values","ios_lproj":"en-SG.lproj","sort_order":1,"status":"active"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router := portalRouterPlatformAdmin(t, "a@you.co", repository.RoleAdmin, true)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(w, r)

			// rejectUnknownParams runs before the (nil) service is ever
			// touched, so a 400 here — rather than a panic — is what proves
			// it ran first.
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "typo")
		})
	}
}

// TestAddLocaleAndPatchLocaleReachTheService drives a real projectsvc.Service
// through the router, the same way TestListProjectsIsNotGatedByPlatformAdmin
// does, so this test is evidence the two routes are wired to the handlers
// and reach AddLocale/UpdateLocale — not just that the middleware chain
// answers 403 when the flag is false and never gets any further.
func TestAddLocaleAndPatchLocaleReachTheService(t *testing.T) {
	u := user("a@you.co", repository.RoleAdmin, repository.StatusActive)
	u.IsPlatformAdmin = true

	router := portalRouterForUser(t, u, func(h *Handler) {
		h.projectSvc = projectsvc.ProvideService(fakeTx{}, fakeProjectRepo{
			projects: []model.Project{{ID: 1, Code: "youtrip", Status: "active"}},
		}, fakeRoleRepo{}, workingFakeLocaleRepo{})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/youtrip/locales",
		strings.NewReader(`{"code":"vi-VN","flutter_dir":"vi_VN","android_values_dir":"values-vi","ios_lproj":"vi-VN.lproj","sort_order":7}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(w, r)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"vi-VN"`)

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/youtrip/locales/vi-VN",
		strings.NewReader(`{"flutter_dir":"vi_VN","android_values_dir":"values-vi","ios_lproj":"vi-VN.lproj","sort_order":7,"status":"archived"}`))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(w2, r2)

	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Contains(t, w2.Body.String(), `"status":"archived"`)
}

// workingFakeLocaleRepo is a minimal in-memory LocaleRepository used only by
// TestAddLocaleAndPatchLocaleReachTheService, which needs Create and Update to
// actually complete rather than panic, to tell "wired to the service" apart
// from "unreachable for an unrelated reason" — the same rationale
// TestListProjectsIsNotGatedByPlatformAdmin gives for fakeProjectRepo above.
type workingFakeLocaleRepo struct{}

func (workingFakeLocaleRepo) List(context.Context, *gorm.DB, int16, bool) ([]model.Locale, error) {
	return nil, errors.New("workingFakeLocaleRepo: List not needed by this test")
}
func (workingFakeLocaleRepo) ByCode(context.Context, *gorm.DB, int16, string) (model.Locale, error) {
	return model.Locale{}, errors.New("workingFakeLocaleRepo: ByCode not needed by this test")
}
func (workingFakeLocaleRepo) Create(_ context.Context, _ *gorm.DB, l model.Locale) (model.Locale, error) {
	l.ID = 99
	l.Status = "active"
	return l, nil
}
func (workingFakeLocaleRepo) Update(_ context.Context, _ *gorm.DB, projectID int16, code string, l model.Locale) (model.Locale, error) {
	l.ID = 99
	l.ProjectID = projectID
	l.Code = code
	return l, nil
}

// --- doubles for a WORKING project service ----------------------------------
//
// Every other test in this file relies on the Handler's services being nil,
// so a request that reaches one panics rather than quietly passing — the
// convention documented on portalRouter. That convention is exactly wrong for
// TestListProjectsIsNotGatedByPlatformAdmin below: a panic and a 403 are both
// "not a 200", so a nil projectSvc could never tell "the route isn't gated"
// apart from "the route panicked for an unrelated reason". These doubles
// exist so that ONE test can drive a real, completing request instead.

type fakeProjectRepo struct{ projects []model.Project }

func (f fakeProjectRepo) ByCode(_ context.Context, _ *gorm.DB, code string) (model.Project, error) {
	for _, p := range f.projects {
		if p.Code == code {
			return p, nil
		}
	}
	return model.Project{}, repository.ErrNotFound
}
func (f fakeProjectRepo) ByID(context.Context, *gorm.DB, int16) (model.Project, error) {
	return model.Project{}, repository.ErrNotFound
}
func (f fakeProjectRepo) List(context.Context, *gorm.DB, bool) ([]model.Project, error) {
	return f.projects, nil
}
func (f fakeProjectRepo) Create(context.Context, *gorm.DB, model.Project) (model.Project, error) {
	return model.Project{}, errors.New("fakeProjectRepo: Create not needed by this test")
}
func (f fakeProjectRepo) Update(context.Context, *gorm.DB, int16, string, string, string) (model.Project, error) {
	return model.Project{}, errors.New("fakeProjectRepo: Update not needed by this test")
}

type fakeRoleRepo struct{}

func (fakeRoleRepo) Grant(context.Context, *gorm.DB, string, int16, string, string) error {
	return errors.New("fakeRoleRepo: Grant not needed by this test")
}

// fakeLocaleRepo satisfies repository.LocaleRepository for tests that build a
// real projectsvc.Service but never intend to reach the locale methods.
type fakeLocaleRepo struct{}

func (fakeLocaleRepo) List(context.Context, *gorm.DB, int16, bool) ([]model.Locale, error) {
	return nil, errors.New("fakeLocaleRepo: List not needed by this test")
}
func (fakeLocaleRepo) ByCode(context.Context, *gorm.DB, int16, string) (model.Locale, error) {
	return model.Locale{}, errors.New("fakeLocaleRepo: ByCode not needed by this test")
}
func (fakeLocaleRepo) Create(context.Context, *gorm.DB, model.Locale) (model.Locale, error) {
	return model.Locale{}, errors.New("fakeLocaleRepo: Create not needed by this test")
}
func (fakeLocaleRepo) Update(context.Context, *gorm.DB, int16, string, model.Locale) (model.Locale, error) {
	return model.Locale{}, errors.New("fakeLocaleRepo: Update not needed by this test")
}

// fakeTx runs the body directly, on no connection at all — every method the
// two fakes above actually exercise ignores the *gorm.DB it is handed.
type fakeTx struct{}

func (fakeTx) WithTransaction(_ context.Context, fn database.TransactionFunc) error {
	return fn(nil)
}

// TestListProjectsIsNotGatedByPlatformAdmin proves GET /projects sits in the
// viewer group, not behind requirePlatformAdmin.
//
// route/key_test.go's own reasoning for driving the real router applies here
// too: a route accidentally mounted in the wrong group is invisible to a test
// of the handler in isolation. If ListProjects ever landed behind
// requirePlatformAdmin, every non-platform-admin operator — which is nearly
// everyone — would be locked out of listing projects at all, and nothing
// else in this suite would notice.
func TestListProjectsIsNotGatedByPlatformAdmin(t *testing.T) {
	u := user("v@you.co", repository.RoleViewer, repository.StatusActive)
	u.IsPlatformAdmin = false

	router := portalRouterForUser(t, u, func(h *Handler) {
		h.projectSvc = projectsvc.ProvideService(fakeTx{}, fakeProjectRepo{}, fakeRoleRepo{}, fakeLocaleRepo{})
	})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	require.NotEqual(t, http.StatusForbidden, w.Code,
		"a plain viewer, not a platform admin, must not be refused here")
	assert.NotContains(t, w.Body.String(), "forbidden")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"projects":[]}`, w.Body.String())
}
