package route

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
