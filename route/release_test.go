package route

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// TestReleaseRoutesEnforceTheirRoleMinimums.
//
// Knowing what shipped is reading; changing what ships is not. A publish puts
// copy in front of customers with no diff reviewed, and a rollback withdraws it
// from every client — both need the role that exists for that judgement.
func TestReleaseRoutesEnforceTheirRoleMinimums(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/releases", `{"notes":"x"}`},
		{http.MethodPost, "/api/v1/releases/1/rollback", ``},
	}
	reads := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/releases"},
		{http.MethodGet, "/api/v1/releases/1"},
		{http.MethodGet, "/api/v1/releases/1/bundles/en-SG"},
	}

	t.Run("an editor cannot publish or roll back", func(t *testing.T) {
		router := portalRouter(t, "e@you.co", repository.RoleEditor)
		for _, rt := range writes {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				assertForbidden(t, router, rt.method, rt.path, rt.body)
			})
		}
	})

	t.Run("nothing is reachable without a token", func(t *testing.T) {
		router := portalRouter(t, "a@you.co", repository.RoleApprover)
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

// TestReleaseRoutesRejectUnknownQueryParameters.
func TestReleaseRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	for _, path := range []string{
		"/api/v1/releases?page=2",
		"/api/v1/releases/1?expand=bundles",
		"/api/v1/releases/1/bundles/en-SG?pretty=true",
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

// TestReleaseVersionPathRefusesNonsense.
//
// Releases are addressed by their human-facing VERSION rather than a row id,
// because that is the number in the incident channel when somebody says "roll
// back 41".
func TestReleaseVersionPathRefusesNonsense(t *testing.T) {
	router := portalRouter(t, "a@you.co", repository.RoleApprover)

	for _, raw := range []string{"0", "-1", "abc", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/releases/"+raw+"/rollback", nil)
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(rec, r)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "version must be a positive integer")
		})
	}
}

// TestReleaseResponseMakesProvenanceAndTheKillSwitchLegible.
func TestReleaseResponseMakesProvenanceAndTheKillSwitchLegible(t *testing.T) {
	t.Run("a merge release names its request", func(t *testing.T) {
		mrID := int64(12)
		body, err := json.Marshal(newReleaseResponse(repository.ReleaseDetail{
			Version: 41, Source: "merge", MergeRequestID: &mrID,
			CreatedAt: time.Now(), LocaleCount: 6,
		}))
		require.NoError(t, err)
		assert.Contains(t, string(body), `"merge_request_id":12`)
		assert.Contains(t, string(body), `"rolled_back":false`)
		assert.Contains(t, string(body), `"rolled_back_by":null`)
	})

	t.Run("a manual publish has no request, and that null is the point", func(t *testing.T) {
		// It is the difference between "somebody approved this" and "somebody
		// pushed it", and the first thing an incident review looks at.
		body, err := json.Marshal(newReleaseResponse(repository.ReleaseDetail{
			Version: 42, Source: "publish", CreatedAt: time.Now(),
		}))
		require.NoError(t, err)
		assert.Contains(t, string(body), `"merge_request_id":null`)
		assert.Contains(t, string(body), `"min_app_version":null`)
	})

	t.Run("a rolled-back release names who pulled the switch", func(t *testing.T) {
		at := time.Date(2026, 8, 8, 3, 14, 0, 0, time.UTC)
		by := "oncall@you.co"
		body, err := json.Marshal(newReleaseResponse(repository.ReleaseDetail{
			Version: 41, Source: "merge", CreatedAt: at,
			RolledBackAt: &at, RolledBackBy: &by,
		}))
		require.NoError(t, err)
		assert.Contains(t, string(body), `"rolled_back":true`)
		assert.Contains(t, string(body), `"rolled_back_by":"oncall@you.co"`)
		assert.Contains(t, string(body), `"rolled_back_at":"2026-08-08T03:14:00Z"`)
	})
}

// TestBundleResponsePassesTheStoredDocumentThrough.
//
// Decoding and re-encoding a 60KB document would change nothing except the risk
// of changing something — and the sha256 alongside it is the fingerprint the OTA
// path serves as an ETag.
func TestBundleResponsePassesTheStoredDocumentThrough(t *testing.T) {
	stored := `{"a":"one","b":""}`

	body, err := json.Marshal(bundleResponse{
		Version: 41, Locale: "en-SG",
		SHA256:   strings.Repeat("a", 64),
		KeyCount: 2, ByteSize: len(stored),
		Strings: json.RawMessage(stored),
	})
	require.NoError(t, err)

	assert.Contains(t, string(body), `"strings":{"a":"one","b":""}`)
	// A deliberately blank value survives the round trip as "" rather than
	// disappearing — the same three-state rule the export depends on.
	assert.Contains(t, string(body), `"b":""`)
}

// TestAlreadyRolledBackIsAConflict.
//
// Not a repeat and not a 500: the switch has already been pulled, and
// rolled_back_by is the answer to the only question anybody asks afterwards.
func TestAlreadyRolledBackIsAConflict(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/releases/41/rollback", nil)

	h.portalError(rec, r, "roll back release",
		fmt.Errorf("release 41: %w", repository.ErrAlreadyRolledBack))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "already_rolled_back")
}

// TestReleaseListResponseIsAlwaysAnArray.
func TestReleaseListResponseIsAlwaysAnArray(t *testing.T) {
	body, err := json.Marshal(releaseListResponse{Releases: []releaseResponse{}, Limit: 50})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"releases":[]`)
}
