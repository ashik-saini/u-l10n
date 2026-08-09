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

	"github.com/yougroupteam/u-common-components/apm"

	"github.com/yougroupteam/u-l10n/pkg/googleauth"
	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
)

// portalRouter builds the real router with a caller of the given role.
//
// Every service on the Handler is deliberately nil. A request that reaches a
// handler panics rather than quietly passing, which is what makes these tests
// evidence that the refusal happened in the middleware and not somewhere
// downstream.
func portalRouter(t *testing.T, email, role string) http.Handler {
	t.Helper()
	return portalRouterForUser(t, user(email, role, repository.StatusActive))
}

// portalRouterPlatformAdmin is portalRouter with control over the global
// privilege. Project creation is the one thing a project admin may not do,
// so the flag has to be settable independently of the role.
func portalRouterPlatformAdmin(t *testing.T, email, role string, isPlatformAdmin bool) http.Handler {
	t.Helper()
	u := user(email, role, repository.StatusActive)
	u.IsPlatformAdmin = isPlatformAdmin
	return portalRouterForUser(t, u)
}

// portalRouterForUser is the construction shared by portalRouter and
// portalRouterPlatformAdmin, factored out so the two cannot drift into
// wiring the stub authenticator two different ways.
//
// mutate is applied to the Handler after it is built and before the router
// is assembled, for the rare test that needs a working service rather than
// the deliberate nil every other caller relies on — see
// TestListProjectsIsNotGatedByPlatformAdmin, which needs ListProjects to
// actually complete rather than panic, to tell "not gated" apart from
// "unreachable for an unrelated reason".
func portalRouterForUser(t *testing.T, u repository.User, mutate ...func(*Handler)) http.Handler {
	t.Helper()

	verifier := &stubVerifier{info: googleauth.TokenInfo{Email: u.Email}}
	users := &stubUsers{byEmail: map[string]repository.User{u.Email: u}}
	handler := identityHandler(verifier, users)
	for _, m := range mutate {
		m(handler)
	}
	return ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)
}

// TestKeyRoutesEnforceTheirRoleMinimums.
//
// The role gate is per-route. Reading the corpus is viewer; changing it is
// editor. A write route accidentally mounted in the read group would be
// invisible in a test of the handler itself, which is why this drives the real
// router.
func TestKeyRoutesEnforceTheirRoleMinimums(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/keys", `{"name":"a","platforms":["flutter"]}`},
		{http.MethodPatch, "/api/v1/keys/1", `{"description":"x"}`},
		{http.MethodDelete, "/api/v1/keys/1", ``},
		{http.MethodPut, "/api/v1/keys/1/translations/en-SG", `{"value":"x","base_version":0}`},
		{http.MethodDelete, "/api/v1/keys/1/translations/en-SG", ``},
	}
	reads := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/keys"},
		{http.MethodGet, "/api/v1/keys/1"},
		{http.MethodGet, "/api/v1/keys/1/history"},
	}

	t.Run("a viewer cannot write", func(t *testing.T) {
		router := portalRouter(t, "v@you.co", repository.RoleViewer)
		for _, rt := range writes {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				r := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
				r.Header.Set("Authorization", "Bearer ya29.good")
				router.ServeHTTP(rec, r)

				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Contains(t, rec.Body.String(), "insufficient_role")
				// A 403 that invites re-authentication is a lie: the caller
				// proved who they are and the answer will not change.
				assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
			})
		}
	})

	t.Run("nobody reads or writes without a token", func(t *testing.T) {
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
				assert.Equal(t, `Bearer realm="u-l10n"`, rec.Header().Get("WWW-Authenticate"))
			})
		}
	})
}

// TestKeyRoutesRejectUnknownQueryParameters.
//
// Checked through the router with a viewer, so the refusal is proved to happen
// before the handler needs a service. A portal that misspells "untranslated_in"
// must be told, not handed the unfiltered corpus.
func TestKeyRoutesRejectUnknownQueryParameters(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	cases := []struct{ name, path string }{
		{"misspelled filter", "/api/v1/keys?untranslated=en-SG"},
		{"unsupported filter", "/api/v1/keys?sort=name"},
		{"branch on the wrong endpoint", "/api/v1/keys/1/history?branch=x"},
		{"typo on a single key", "/api/v1/keys/1?locale=en-SG"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Header.Set("Authorization", "Bearer ya29.good")
			router.ServeHTTP(rec, r)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "unknown query parameter")
		})
	}
}

func TestRejectUnknownParams(t *testing.T) {
	t.Run("accepts what is listed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/keys?branch=x&locales=en-SG", nil)
		assert.NoError(t, rejectUnknownParams(r, "branch", "locales", "tag"))
	})

	t.Run("names the offender and the alternatives", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/keys?brnach=x", nil)
		err := rejectUnknownParams(r, "branch", "locales")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"brnach"`)
		assert.Contains(t, err.Error(), "branch, locales")
	})
}

func TestQueryScalars(t *testing.T) {
	t.Run("an absent int uses the fallback", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/keys", nil)
		n, err := queryInt(r, "limit", 100)
		require.NoError(t, err)
		assert.Equal(t, 100, n)
	})

	// A caller who sent limit=abc did not mean "use the default". Silently
	// falling back hides the bug in their code.
	for _, raw := range []string{"abc", "-1", "1.5", ""} {
		t.Run("limit="+raw+" is refused", func(t *testing.T) {
			if raw == "" {
				t.Skip("an omitted parameter is the fallback case above")
			}
			r := httptest.NewRequest(http.MethodGet, "/keys?limit="+raw, nil)
			_, err := queryInt(r, "limit", 0)
			assert.Error(t, err)
		})
	}

	t.Run("booleans are strict", func(t *testing.T) {
		for raw, want := range map[string]bool{"true": true, "false": false} {
			r := httptest.NewRequest(http.MethodGet, "/keys?include_deleted="+raw, nil)
			got, err := queryBool(r, "include_deleted")
			require.NoError(t, err)
			assert.Equal(t, want, got)
		}
		// Accepting a loose vocabulary means accepting "flase" as false.
		for _, raw := range []string{"1", "yes", "TRUE", "flase"} {
			r := httptest.NewRequest(http.MethodGet, "/keys?include_deleted="+raw, nil)
			_, err := queryBool(r, "include_deleted")
			assert.Error(t, err, raw)
		}
	})

	t.Run("lists tolerate stray commas", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/keys?locales=en-SG,,ms-MY,", nil)
		assert.Equal(t, []string{"en-SG", "ms-MY"}, queryList(r, "locales"))
		r = httptest.NewRequest(http.MethodGet, "/keys", nil)
		assert.Nil(t, queryList(r, "locales"))
	})
}

// TestCellResponseKeepsTheThreeStatesApart.
//
// This is the three-state rule at the JSON boundary, and it is the one place
// where a plain `string` field would silently destroy it: an untranslated cell
// and a deliberately blank one would both render as "value":"".
func TestCellResponseKeepsTheThreeStatesApart(t *testing.T) {
	encode := func(c repository.Cell) string {
		body, err := json.Marshal(newCellResponse(c))
		require.NoError(t, err)
		return string(body)
	}

	t.Run("untranslated carries no value at all", func(t *testing.T) {
		body := encode(repository.Cell{Found: false})
		assert.Contains(t, body, `"translated":false`)
		assert.NotContains(t, body, `"value"`,
			"an absent value must be absent, not an empty string")
	})

	t.Run("deliberately blank carries an empty value", func(t *testing.T) {
		body := encode(repository.Cell{Found: true, Value: "", Version: 3,
			RenderHint: model.RenderHintPlain})
		assert.Contains(t, body, `"translated":true`)
		assert.Contains(t, body, `"value":""`)
	})

	t.Run("translated carries its text", func(t *testing.T) {
		body := encode(repository.Cell{Found: true, Value: "Top up", Version: 7})
		assert.Contains(t, body, `"value":"Top up"`)
		assert.Contains(t, body, `"version":7`)
	})

	t.Run("a branch override is marked", func(t *testing.T) {
		body := encode(repository.Cell{Found: true, Value: "x", FromBranch: true})
		assert.Contains(t, body, `"from_branch":true`)
	})
}

// TestTranslationConflictBodyCarriesBothValues.
//
// The whole reason for answering 409 rather than retrying is that a human has
// to choose. A body saying only "conflict" forces the portal into a second read
// to render the dialog — and that read can return a third value.
func TestTranslationConflictBodyCarriesBothValues(t *testing.T) {
	h := &Handler{}
	updatedAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/keys/12/translations/en-SG", nil)

	h.translationError(rec, r, "set translation", true, &keysvc.ConflictError{
		KeyID:           12,
		Locale:          "en-SG",
		Mine:            "my late edit",
		ExpectedVersion: 1,
		Theirs: repository.Cell{
			Found: true, Value: "theirs v2", Version: 2,
			UpdatedBy: "someone@you.co", UpdatedAt: &updatedAt,
		},
	})

	require.Equal(t, http.StatusConflict, rec.Code)

	var body conflictResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, "version_conflict", body.Error)
	assert.Equal(t, int64(12), body.KeyID)
	assert.Equal(t, "en-SG", body.Locale)
	assert.Equal(t, 1, body.BaseVersion)

	require.NotNil(t, body.Mine)
	assert.Equal(t, "my late edit", *body.Mine)

	require.NotNil(t, body.Theirs.Value)
	assert.Equal(t, "theirs v2", *body.Theirs.Value)
	assert.Equal(t, 2, body.Theirs.Version)
	assert.Equal(t, "someone@you.co", body.Theirs.UpdatedBy)
}

// TestTranslationConflictDistinguishesDeletedFromBlanked.
//
// "Somebody deleted the translation you were editing" and "somebody blanked it"
// are different answers, and a translator resolves them differently.
func TestTranslationConflictDistinguishesDeletedFromBlanked(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/keys/12/translations/en-SG", nil)

	h.translationError(rec, r, "set translation", true, &keysvc.ConflictError{
		KeyID: 12, Locale: "en-SG", Mine: "mine", ExpectedVersion: 4,
		Theirs: repository.Cell{Found: false},
	})

	require.Equal(t, http.StatusConflict, rec.Code)

	var body conflictResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.Theirs.Translated)
	assert.Nil(t, body.Theirs.Value, "there is nothing stored to show")
}

// TestPortalErrorStatusCodes pins the sentinel-to-status mapping.
//
// Getting this wrong is quiet and expensive in both directions: a caller error
// returned as 500 sends an on-call engineer after a fault that does not exist,
// and an infrastructure failure returned as 4xx hides a real outage behind what
// looks like a client bug.
func TestPortalErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"unknown locale", fmt.Errorf("%w: unknown locale", keysvc.ErrBadRequest),
			http.StatusBadRequest, "bad_request"},
		{"missing key", fmt.Errorf("key 9: %w", repository.ErrNotFound),
			http.StatusNotFound, "not_found"},
		{"lost race", fmt.Errorf("x: %w", repository.ErrOptimisticLock),
			http.StatusConflict, "version_conflict"},
		{"duplicate key name", fmt.Errorf("x: %w", repository.ErrKeyNameTaken),
			http.StatusConflict, "name_taken"},
		{"merged branch", fmt.Errorf("%w: merged", keysvc.ErrBranchNotOpen),
			http.StatusConflict, "branch_not_open"},
		{"database down", fmt.Errorf("dial tcp: connection refused"),
			http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)

			h.portalError(rec, r, "list keys", tc.err)

			assert.Equal(t, tc.want, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.code)
		})
	}
}

// TestPortalErrorDoesNotLeakInternalDetail: a 500 says nothing about why. The
// reason is in the log, where it belongs.
func TestPortalErrorDoesNotLeakInternalDetail(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)

	h.portalError(rec, r, "list keys",
		fmt.Errorf("host=db.internal user=l10n password=hunter2"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "hunter2")
	assert.NotContains(t, rec.Body.String(), "db.internal")
}

// TestOptionalStringHasThreeStates.
//
// android_name NULL means "derive it from the key name". Absent means "leave it
// alone". A plain *string cannot tell those apart, and collapsing them makes an
// override impossible to remove through the API.
func TestOptionalStringHasThreeStates(t *testing.T) {
	decode := func(body string) patchKeyRequest {
		var got patchKeyRequest
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		return got
	}

	t.Run("absent", func(t *testing.T) {
		got := decode(`{"description":"x"}`)
		assert.False(t, got.AndroidName.Set)
	})

	t.Run("explicitly null", func(t *testing.T) {
		got := decode(`{"android_name":null}`)
		assert.True(t, got.AndroidName.Set)
		assert.Nil(t, got.AndroidName.Value)
	})

	t.Run("a value", func(t *testing.T) {
		got := decode(`{"android_name":"custom"}`)
		require.True(t, got.AndroidName.Set)
		require.NotNil(t, got.AndroidName.Value)
		assert.Equal(t, "custom", *got.AndroidName.Value)
	})
}

// TestPutTranslationRefusesAnOmittedValue.
//
// Writing "" is a deliberate act — it means "this string is intentionally
// blank" — and it must not be what a client gets for forgetting a field.
func TestPutTranslationRefusesAnOmittedValue(t *testing.T) {
	router := portalRouter(t, "e@you.co", repository.RoleEditor)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/keys/1/translations/en-SG",
		strings.NewReader(`{"base_version":0}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "value is required")
	assert.Contains(t, rec.Body.String(), "DELETE")
}

// TestPutTranslationRejectsAnUnknownField: a misspelled field must be an error,
// not a silently defaulted zero.
func TestPutTranslationRejectsAnUnknownField(t *testing.T) {
	router := portalRouter(t, "e@you.co", repository.RoleEditor)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/keys/1/translations/en-SG",
		strings.NewReader(`{"value":"x","baseVersion":1}`))
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "baseVersion")
}

// TestBrowseLimitIsCapped: the cap sits above the corpus so the browser can
// still fetch every key in one request, and exists only to refuse a number the
// response could not be assembled for.
func TestBrowseLimitIsCapped(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/keys?limit=1000000", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Greater(t, maxBrowseLimit, 6300, "the whole corpus must fit in one page")
}

// TestKeyHistoryLimitIsCapped: the history limit reaches the database verbatim,
// so an unbounded one is a caller-chosen query cost. Same rule as the browser's
// maxBrowseLimit — refuse rather than truncate.
func TestKeyHistoryLimitIsCapped(t *testing.T) {
	router := portalRouter(t, "v@you.co", repository.RoleViewer)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/keys/1/history?limit=2000000000", nil)
	r.Header.Set("Authorization", "Bearer ya29.good")
	router.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "limit")
}
