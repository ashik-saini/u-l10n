package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TestBranchRoutesEnforceTheirRoleMinimums.
//
// Reading what work is in flight is viewer; opening and closing workspaces is
// editor. The services on the Handler are nil, so a request that slips past the
// middleware panics rather than quietly passing.
func TestBranchRoutesEnforceTheirRoleMinimums(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/branches", `{"name":"x"}`},
		{http.MethodPost, "/api/v1/branches/x/close", ``},
		{http.MethodPost, "/api/v1/branches/x/reopen", ``},
	}
	reads := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/branches"},
		{http.MethodGet, "/api/v1/branches/x"},
		{http.MethodGet, "/api/v1/branches/x/changes"},
	}

	t.Run("a viewer cannot open or close a branch", func(t *testing.T) {
		router := portalRouter(t, "v@you.co", repository.RoleViewer)
		for _, rt := range writes {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				r := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
				r.Header.Set("Authorization", "Bearer ya29.good")
				router.ServeHTTP(rec, r)

				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Contains(t, rec.Body.String(), "insufficient_role")
			})
		}
	})

	t.Run("nothing is reachable without a token", func(t *testing.T) {
		router := portalRouter(t, "e@you.co", repository.RoleEditor)
		all := append([]struct{ method, path string }{}, reads...)
		for _, rt := range writes {
			all = append(all, struct{ method, path string }{rt.method, rt.path})
		}

		for _, rt := range all {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
			})
		}
	})
}

// TestBranchRoutesRejectUnknownQueryParameters.
func TestBranchRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	for _, path := range []string{
		"/api/v1/branches?state=open",
		"/api/v1/branches/x?verbose=true",
		"/api/v1/branches/x/changes?conflicts_only=true",
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

// TestBranchChangesResponseKeepsBothSidesThreeStated.
//
// A reviewer decides between two values, and "there is no value" is one of the
// options on each side. Rendering an absent master row and a blank master row
// identically would remove the distinction the merge acts on.
func TestBranchChangesResponseKeepsBothSidesThreeStated(t *testing.T) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	body, err := json.Marshal(newBranchChangesResponse(
		repository.Branch{Name: "copy-fixes", Status: "open"},
		repository.BranchChanges{
			Values: []repository.BranchValueChange{
				{
					KeyID: 1, KeyName: "a", LocaleCode: "en-SG",
					Value: "new copy", MasterValue: "old copy", MasterFound: true,
					BaseMasterVersion: 1, MasterVersion: 2, Conflict: true,
					UpdatedAt: now,
				},
				{
					// A tombstone: the merge will DELETE master's row.
					KeyID: 2, KeyName: "b", LocaleCode: "ms-MY",
					Removed: true, MasterValue: "goes away", MasterFound: true,
					UpdatedAt: now,
				},
				{
					// A new value where master has no row at all.
					KeyID: 3, KeyName: "c", LocaleCode: "th-TH",
					Value: "", MasterFound: false, UpdatedAt: now,
				},
			},
			Meta: []repository.BranchMetaChange{
				{Name: "renamed", MasterName: "original", Conflict: true, UpdatedAt: now},
			},
		}))
	require.NoError(t, err)

	var got branchChangesResponse
	require.NoError(t, json.Unmarshal(body, &got))

	require.Len(t, got.Values, 3)

	assert.False(t, got.Values[0].Removed)
	require.NotNil(t, got.Values[0].Value)
	assert.Equal(t, "new copy", *got.Values[0].Value)
	require.NotNil(t, got.Values[0].MasterValue)
	assert.Equal(t, "old copy", *got.Values[0].MasterValue)

	assert.True(t, got.Values[1].Removed)
	assert.Nil(t, got.Values[1].Value, "a removal carries no value")

	assert.False(t, got.Values[2].MasterTranslated)
	assert.Nil(t, got.Values[2].MasterValue, "master has no row to show")
	require.NotNil(t, got.Values[2].Value)
	assert.Equal(t, "", *got.Values[2].Value, "a deliberate blank IS a value")

	// Both lists contribute, so the portal can render "2 conflicts" without
	// walking either.
	assert.Equal(t, 2, got.Conflicts)
}

// TestBranchListResponseIsAlwaysAnArray: an empty list must serialise as [] and
// not null, so the portal needs no null check.
func TestBranchListResponseIsAlwaysAnArray(t *testing.T) {
	body, err := json.Marshal(branchListResponse{Branches: []branchResponse{}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"branches":[]}`, string(body))

	body, err = json.Marshal(newBranchChangesResponse(
		repository.Branch{Name: "empty"}, repository.BranchChanges{}))
	require.NoError(t, err)
	assert.Contains(t, string(body), `"values":[]`)
	assert.Contains(t, string(body), `"meta":[]`)
}

// TestBranchResponseNullsAreMeaningful.
func TestBranchResponseNullsAreMeaningful(t *testing.T) {
	body, err := json.Marshal(newBranchResponse(repository.BranchSummary{
		Branch: repository.Branch{Name: "fresh", Status: "open"},
	}))
	require.NoError(t, err)

	// A branch nobody has written to has no last_edited_at, and that is what
	// makes its approval un-invalidatable — not a zero timestamp.
	assert.Contains(t, string(body), `"last_edited_at":null`)
	assert.Contains(t, string(body), `"merge_request_id":null`)
}
