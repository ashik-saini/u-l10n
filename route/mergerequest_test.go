package route

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
)

// TestMergeRequestRoutesEnforceTheirRoleMinimums.
//
// The split is the point of the approver role existing at all: an editor may
// propose, withdraw and decide which side of a conflict wins, but only an
// approver may sign the diff off and put it in front of customers.
func TestMergeRequestRoutesEnforceTheirRoleMinimums(t *testing.T) {
	editorRoutes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/merge-requests", `{"branch":"b","title":"t"}`},
		{http.MethodPost, "/api/v1/merge-requests/1/reopen", ``},
		{http.MethodPost, "/api/v1/merge-requests/1/close", ``},
		{http.MethodPut, "/api/v1/merge-requests/1/resolutions", `{"resolutions":[]}`},
	}
	approverRoutes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/merge-requests/1/approve", ``},
		{http.MethodPost, "/api/v1/merge-requests/1/request-changes", `{"comment":"x"}`},
		{http.MethodPost, "/api/v1/merge-requests/1/reject", ``},
		{http.MethodPost, "/api/v1/merge-requests/1/merge", ``},
	}

	t.Run("a viewer cannot propose", func(t *testing.T) {
		router := portalRouter(t, "v@you.co", repository.RoleViewer)
		for _, rt := range editorRoutes {
			t.Run(rt.path, func(t *testing.T) {
				assertForbidden(t, router, rt.method, rt.path, rt.body)
			})
		}
	})

	t.Run("an editor cannot approve or merge", func(t *testing.T) {
		// THE role boundary. An editor who could merge could ship their own copy
		// to every customer without anybody else reading it.
		router := portalRouter(t, "e@you.co", repository.RoleEditor)
		for _, rt := range approverRoutes {
			t.Run(rt.path, func(t *testing.T) {
				assertForbidden(t, router, rt.method, rt.path, rt.body)
			})
		}
	})

	t.Run("nothing is reachable without a token", func(t *testing.T) {
		router := portalRouter(t, "a@you.co", repository.RoleApprover)
		paths := []struct{ method, path string }{
			{http.MethodGet, "/api/v1/merge-requests"},
			{http.MethodGet, "/api/v1/merge-requests/1"},
			{http.MethodGet, "/api/v1/merge-requests/1/conflicts"},
		}
		for _, rt := range append(editorRoutes, approverRoutes...) {
			paths = append(paths, struct{ method, path string }{rt.method, rt.path})
		}

		for _, rt := range paths {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
			})
		}
	})
}

func assertForbidden(t *testing.T, router http.Handler, method, path, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "insufficient_role")
}

// TestMergeRequestRoutesRejectUnknownQueryParameters.
func TestMergeRequestRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	for _, path := range []string{
		"/api/v1/merge-requests?state=open",
		"/api/v1/merge-requests/1?include=events",
		"/api/v1/merge-requests/1/conflicts?kind=values",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(rec, r)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "unknown query parameter")
		})
	}
}

// TestMergeRefusalsAreAllConflictsWithSomethingToActOn.
//
// Every one of these is an expected outcome of a review workflow. A 500 for any
// of them would page an engineer because two translators edited the same string
// — and would tell the reviewer nothing about what to do next.
func TestMergeRefusalsAreAllConflictsWithSomethingToActOn(t *testing.T) {
	t.Run("stale approval", func(t *testing.T) {
		rec := runMergeError(t, fmt.Errorf("%w", mergesvc.ErrStaleApproval))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "stale_approval")
	})

	t.Run("not approved", func(t *testing.T) {
		rec := runMergeError(t, fmt.Errorf("%w (status %q)", mergesvc.ErrNotApproved, "open"))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "not_approved")
	})

	t.Run("unresolved conflicts carry the rows", func(t *testing.T) {
		rec := runMergeError(t, &mergesvc.UnresolvedError{
			Values: []repository.Conflict{{
				KeyID: 7, KeyName: "wallet_cta", LocaleCode: "en-SG",
				Mine: "Top up", Theirs: "Add money", TheirsFound: true,
				BaseMasterVersion: 1, MasterVersion: 2,
			}},
			Meta: []repository.MetaConflict{{
				KeyID: 8, MineName: "a", TheirsName: "b",
			}},
		})

		require.Equal(t, http.StatusConflict, rec.Code)

		var body unresolvedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "unresolved_conflicts", body.Error)

		// The blocking ROWS, not a count: "there were 2 conflicts" is not
		// something a reviewer can act on.
		require.Len(t, body.Values, 1)
		require.NotNil(t, body.Values[0].Mine)
		assert.Equal(t, "Top up", *body.Values[0].Mine)
		require.NotNil(t, body.Values[0].Theirs)
		assert.Equal(t, "Add money", *body.Values[0].Theirs)
		require.Len(t, body.Meta, 1)
	})

	t.Run("name collisions carry the names", func(t *testing.T) {
		rec := runMergeError(t, &mergesvc.CollisionError{
			Collisions: []repository.NameCollision{
				{Name: "login_button", BranchKeyID: 4, MasterKeyID: 9},
			},
		})

		require.Equal(t, http.StatusConflict, rec.Code)

		var body collisionResponseBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "name_collision", body.Error)
		require.Len(t, body.Collisions, 1)
		assert.Equal(t, "login_button", body.Collisions[0].Name)
		assert.Equal(t, int64(9), body.Collisions[0].MasterKeyID)
	})

	t.Run("a concurrent master write is a retryable 409", func(t *testing.T) {
		// The merge rolled back whole; nothing was applied. Retrying surfaces
		// the new conflict for a human — a 500 would page an engineer for a
		// race the workflow is built to absorb.
		rec := runMergeError(t, fmt.Errorf("%w: 1 value delta(s) no longer match master",
			mergesvc.ErrConcurrentMasterWrite))
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "concurrent_master_write")
		assert.Contains(t, rec.Body.String(), "retry",
			"the body must tell the caller the merge is safe to retry")
	})

	t.Run("an unreachable database is still a 500", func(t *testing.T) {
		// The mapping must not turn everything into a 409: a real outage hiding
		// behind a 4xx is exactly as expensive as the reverse.
		rec := runMergeError(t, fmt.Errorf("dial tcp: connection refused"))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

func runMergeError(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/merge-requests/1/merge", nil)
	h.mergeError(rec, r, err)
	return rec
}

// TestWorkflowRefusalsAreConflictsNotNotFound.
//
// The request is right there on the reviewer's screen; it is its STATE that
// refuses. A 404 would send them looking for something they can see.
func TestWorkflowRefusalsAreConflictsNotNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"approving a merged request", fmt.Errorf("%w: merged", mrsvc.ErrNotLive), "merge_request_not_live"},
		{"reopening into a live one", fmt.Errorf("x: %w", repository.ErrLiveMergeRequestExists), "live_merge_request_exists"},
		// The repository-level CAS miss, unwrapped: the backstop for the merge
		// transaction's own approved → merged move losing a race.
		{"a guarded transition that lost its race",
			fmt.Errorf("x: %w", repository.ErrStaleMergeRequestStatus), "merge_request_not_live"},
		// Two publishes picked the same version; the loser retries.
		{"a publish version race",
			fmt.Errorf("x: %w", repository.ErrReleaseVersionRace), "release_version_race"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/merge-requests/1/approve", nil)

			h.portalError(rec, r, "approve", tc.err)

			assert.Equal(t, http.StatusConflict, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.code)
		})
	}
}

// TestConflictsResponseKeepsAllThreeKindsApart.
//
// They are not variations on a theme. Values and metadata are decided by
// choosing a side; a name collision cannot be, which is why it carries no
// resolution field and is excluded from the unresolved count.
func TestConflictsResponseKeepsAllThreeKindsApart(t *testing.T) {
	body, err := json.Marshal(newConflictsResponse(mrsvc.Conflicts{
		Values: []repository.Conflict{
			{KeyID: 1, LocaleCode: "en-SG", Mine: "a", Theirs: "b", TheirsFound: true},
			// The branch removes it; master still has a row.
			{KeyID: 2, LocaleCode: "ms-MY", MineRemoved: true, Theirs: "keep", TheirsFound: true},
			// The branch adds one; master has no row at all.
			{KeyID: 3, LocaleCode: "th-TH", Mine: "new", TheirsFound: false, Resolution: "mine"},
		},
		Meta:       []repository.MetaConflict{{KeyID: 4, MineName: "x", TheirsName: "y"}},
		Collisions: []repository.NameCollision{{Name: "taken", MasterKeyID: 5}},
		Unresolved: 3,
	}))
	require.NoError(t, err)

	var got conflictsResponse
	require.NoError(t, json.Unmarshal(body, &got))

	require.Len(t, got.Values, 3)
	assert.NotNil(t, got.Values[0].Mine)
	assert.NotNil(t, got.Values[0].Theirs)

	assert.True(t, got.Values[1].MineRemoved)
	assert.Nil(t, got.Values[1].Mine, "a removal has no value of its own")

	assert.False(t, got.Values[2].TheirsTranslated)
	assert.Nil(t, got.Values[2].Theirs, "master has no row to choose")
	assert.Equal(t, "mine", got.Values[2].Resolution)

	require.Len(t, got.Collisions, 1)
	assert.Equal(t, "taken", got.Collisions[0].Name)

	assert.Equal(t, 3, got.Unresolved)
	assert.False(t, got.Mergeable, "a collision blocks the merge even with nothing unresolved")
}

// TestMergeableIsFalseWhileACollisionStands.
func TestMergeableIsFalseWhileACollisionStands(t *testing.T) {
	clean := mrsvc.Conflicts{}
	assert.True(t, clean.Mergeable())

	withCollision := mrsvc.Conflicts{
		Collisions: []repository.NameCollision{{Name: "taken"}},
	}
	assert.False(t, withCollision.Mergeable(),
		"no decision clears a collision — one of the two keys has to be renamed")
}

// TestCreateMergeRequestRequiresABranch.
func TestCreateMergeRequestRequiresABranch(t *testing.T) {
	router := portalRouter(t, "e@you.co", repository.RoleEditor)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/merge-requests",
		strings.NewReader(`{"title":"no branch"}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "branch is required")
}

// TestReviewRejectsAMalformedBody.
//
// The comment is optional on four of the five transitions, so an empty body is
// normal — but a malformed one must not be silently dropped, or a reviewer's
// reason vanishes without any sign it did.
func TestReviewRejectsAMalformedBody(t *testing.T) {
	router := portalRouter(t, "a@you.co", repository.RoleApprover)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/merge-requests/1/request-changes",
		strings.NewReader(`{"reason":"typo in the field name"}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "reason")
}
