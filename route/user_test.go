package route

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi"
	"github.com/stretchr/testify/assert"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
)

// TestUserErrorStatusCodes pins the sentinel-to-status mapping.
//
// A caller error returned as 500 sends an on-call engineer after a fault that
// does not exist; an infrastructure failure returned as 4xx buries a real
// outage behind what looks like a client bug.
func TestUserErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"unknown role", fmt.Errorf("%w: role must be", usersvc.ErrBadRequest), http.StatusBadRequest, "bad_request"},
		{"no such user", fmt.Errorf("user %q: %w", "x@you.co", repository.ErrNotFound), http.StatusNotFound, "not_found"},
		{"database down", errors.New("dial tcp: connection refused"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/x@you.co/role", nil)

			(&Handler{}).userError(w, r, "set role", tc.err)

			assert.Equal(t, tc.want, w.Code)
			assert.Contains(t, w.Body.String(), tc.code)
		})
	}
}

// TestUserErrorDoesNotLeakInternalDetail: a 500 says nothing about why. The
// reason is in the log, where it belongs.
func TestUserErrorDoesNotLeakInternalDetail(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/users/x@you.co/role", nil)

	(&Handler{}).userError(w, r, "set role",
		errors.New("host=db.internal user=l10n password=hunter2"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "hunter2")
	assert.NotContains(t, w.Body.String(), "db.internal")
}

func TestPathEmail(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		// @ and . are legal in a path segment, so the common case arrives
		// untouched.
		{"ashik.saini@you.co", "ashik.saini@you.co", false},
		// A client that percent-encodes it must get the same answer.
		{"ashik.saini%40you.co", "ashik.saini@you.co", false},
		{"a%2Bb@you.co", "a+b@you.co", false},
		{"", "", true},
		// A truncated escape is a URL the client built wrongly, and must be
		// reported as that rather than becoming a confusing "no such user".
		{"a%zz@you.co", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("email", tc.raw)
			r := httptest.NewRequest(http.MethodPatch, "/", nil).
				WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

			got, err := pathEmail(r)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
