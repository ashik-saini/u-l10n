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

func (f fakeProjectRepo) ByCode(context.Context, *gorm.DB, string) (model.Project, error) {
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
		h.projectSvc = projectsvc.ProvideService(fakeTx{}, fakeProjectRepo{}, fakeRoleRepo{})
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
