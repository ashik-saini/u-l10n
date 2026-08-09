package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TestTagRoutesEnforceTheirRoleMinimums.
func TestTagRoutesEnforceTheirRoleMinimums(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/tags", `{"name":"x"}`},
		{http.MethodPut, "/api/v1/tags/1", `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/tags/1", ``},
		{http.MethodPost, "/api/v1/tags/1/keys", `{"key_ids":[1]}`},
		{http.MethodDelete, "/api/v1/tags/1/keys", `{"key_ids":[1]}`},
		{http.MethodPut, "/api/v1/keys/1/tags", `{"tag_ids":[1]}`},
	}

	t.Run("a viewer cannot change labels", func(t *testing.T) {
		router := portalRouter(t, "v@you.co", repository.RoleViewer)
		for _, rt := range writes {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				assertForbidden(t, router, rt.method, rt.path, rt.body)
			})
		}
	})

	t.Run("nothing is reachable without a token", func(t *testing.T) {
		router := portalRouter(t, "e@you.co", repository.RoleEditor)
		paths := []struct{ method, path string }{{http.MethodGet, "/api/v1/tags"}}
		for _, rt := range writes {
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

// TestTagRoutesRejectUnknownQueryParameters.
//
// Tags are global per key rather than branch-scoped — that is what keeps them
// out of conflict computation entirely — so ?branch= on a tag route is a
// misunderstanding worth reporting rather than ignoring.
func TestTagRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tags?branch=copy-fixes", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown query parameter")
}

// TestPutKeyTagsRefusesAnOmittedList.
//
// An omitted field must not be read as "clear everything". Clearing is a
// deliberate act and it has its own spelling: [].
func TestPutKeyTagsRefusesAnOmittedList(t *testing.T) {
	router := portalRouter(t, "e@you.co", repository.RoleEditor)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/keys/1/tags", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "tag_ids is required")
	assert.Contains(t, rec.Body.String(), "[]")
}

// TestPathTagIDRefusesWhatWouldOverflow.
//
// tags.id is a SMALLSERIAL. A value above 32767 parsed into an int16 would wrap
// and address a different tag — silently, and destructively on DELETE.
func TestPathTagIDRefusesWhatWouldOverflow(t *testing.T) {
	router := portalRouter(t, "e@you.co", repository.RoleEditor)

	for _, raw := range []string{"32768", "99999", "0", "-1", "abc"} {
		t.Run(raw, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/api/v1/tags/"+raw, nil)
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(rec, r)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "tag id")
		})
	}
}

// TestBulkTagResponseOnlyReportsRemovedForAnUnassign.
//
// An assign is ON CONFLICT DO NOTHING, so a retried request would report
// "0 added" and read as a failure. Absence is the honest answer there.
func TestBulkTagResponseOnlyReportsRemovedForAnUnassign(t *testing.T) {
	removed := int64(40)

	unassign, err := json.Marshal(bulkTagResponse{
		Tag: tagResponse{ID: 1, Name: "needs-review"}, Requested: 50, Removed: &removed,
	})
	require.NoError(t, err)
	assert.Contains(t, string(unassign), `"removed":40`)

	assign, err := json.Marshal(bulkTagResponse{
		Tag: tagResponse{ID: 1, Name: "needs-review"}, Requested: 50,
	})
	require.NoError(t, err)
	assert.NotContains(t, string(assign), `"removed"`)
}

// TestTagListResponseIsAlwaysAnArray.
func TestTagListResponseIsAlwaysAnArray(t *testing.T) {
	body, err := json.Marshal(tagListResponse{Tags: []tagUsageResponse{}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"tags":[]}`, string(body))

	// And a key with no tags renders [] rather than null, so the portal needs no
	// null check per row.
	body, err = json.Marshal(keyTagsResponse{KeyID: 1, Tags: newTagResponses(nil)})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"tags":[]`)
}

// TestDeleteTagReportsTheCascade.
//
// "Deleted the tag" and "deleted the tag and detached it from 812 keys" are very
// different outcomes for the person who clicked, so this is not a bare 204.
func TestDeleteTagReportsTheCascade(t *testing.T) {
	body, err := json.Marshal(deleteTagResponse{DetachedKeys: 812})
	require.NoError(t, err)
	assert.JSONEq(t, `{"detached_keys":812}`, string(body))
}
