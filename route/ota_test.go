package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi"
	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// stubLocales is a LocaleRepository backed by a map.
type stubLocales struct {
	byCode map[string]model.Locale
}

func (s *stubLocales) List(context.Context, *gorm.DB, int16, bool) ([]model.Locale, error) {
	out := make([]model.Locale, 0, len(s.byCode))
	for _, l := range s.byCode {
		out = append(out, l)
	}
	return out, nil
}

func (s *stubLocales) ByCode(_ context.Context, _ *gorm.DB, _ int16, code string) (model.Locale, error) {
	l, ok := s.byCode[code]
	if !ok {
		return model.Locale{}, fmt.Errorf("locale %q: %w", code, repository.ErrNotFound)
	}
	return l, nil
}

func (s *stubLocales) Create(context.Context, *gorm.DB, model.Locale) (model.Locale, error) {
	return model.Locale{}, fmt.Errorf("stubLocales: Create not needed by this test")
}

func (s *stubLocales) Update(context.Context, *gorm.DB, int16, string, model.Locale) (model.Locale, error) {
	return model.Locale{}, fmt.Errorf("stubLocales: Update not needed by this test")
}

// stubReleases records what ServableBundle was asked. The embedded interface
// is nil, so any other method panics rather than quietly passing — the same
// doctrine as the nil services on portalRouter's Handler.
type stubReleases struct {
	repository.ReleaseRepository
	bundle repository.ServableBundle
	err    error
	// appVersions records the appVersion argument of every call, so a test can
	// prove what the handler let through to the SQL layer.
	appVersions []string
}

func (s *stubReleases) ServableBundle(
	_ context.Context, _ *gorm.DB, _ int16, appVersion string,
) (repository.ServableBundle, error) {
	s.appVersions = append(s.appVersions, appVersion)
	return s.bundle, s.err
}

// otaRequest drives OTABundle directly with the {locale} path parameter set.
func otaRequest(h *Handler, target, appVersion string) *httptest.ResponseRecorder {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("locale", "en-SG")

	r := httptest.NewRequest(http.MethodGet, target, nil).
		WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	if appVersion != "" {
		r.Header.Set("X-App-Version", appVersion)
	}

	rec := httptest.NewRecorder()
	h.OTABundle(rec, r)
	return rec
}

func otaHandler(releases *stubReleases) *Handler {
	return &Handler{
		locales: &stubLocales{byCode: map[string]model.Locale{
			// Status is explicit because the handler now reads it: a locale
			// with an unset status is not active, so leaving it zero would 404
			// every test in this file.
			"en-SG": {ID: 1, Code: "en-SG", Status: repository.LocaleActive},
		}},
		releases: releases,
	}
}

// TestOTABundleToleratesAMalformedAppVersion.
//
// The header flows into an int[] cast in servableBundleSQL, so anything that
// is not exactly three numeric components — "4.12.0-beta", "4.12.0 (1234)" —
// would raise a Postgres cast error and 500 the unauthenticated hot path.
// The handler must sanitise it to "" (the documented conservative 0.0.0
// reading) rather than forward it.
func TestOTABundleToleratesAMalformedAppVersion(t *testing.T) {
	cases := []struct {
		name, header, forwarded string
	}{
		{"well-formed", "4.12.0", "4.12.0"},
		{"pre-release suffix", "4.12.0-beta", ""},
		{"build number in parens", "4.12.0 (1234)", ""},
		{"two components", "4.12", ""},
		{"four components", "4.12.0.1", ""},
		{"not a version at all", "latest", ""},
		{"sql-shaped junk", "1.2.3'; --", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			releases := &stubReleases{bundle: repository.ServableBundle{
				ReleaseVersion: 41, Strings: []byte(`{"a":"b"}`), SHA256: "abc",
			}}
			rec := otaRequest(otaHandler(releases), "/ota/v1/bundles/en-SG", tc.header)

			assert.Equal(t, http.StatusOK, rec.Code,
				"a malformed version header is the client's quirk, never a 500")
			require.Len(t, releases.appVersions, 1)
			assert.Equal(t, tc.forwarded, releases.appVersions[0],
				"only a strict N.N.N may reach the int[] cast")
		})
	}
}

// TestOTABundleNegativeResponsesAreCacheable.
//
// Without an explicit Cache-Control the CDN's negative caching is whatever its
// defaults say — nondeterministic across CDNs and config changes. A short
// explicit TTL keeps a storm of misses off the origin without pinning a wrong
// answer for long.
func TestOTABundleNegativeResponsesAreCacheable(t *testing.T) {
	t.Run("unknown locale is a cacheable 404", func(t *testing.T) {
		h := &Handler{locales: &stubLocales{byCode: map[string]model.Locale{}}}
		rec := otaRequest(h, "/ota/v1/bundles/en-SG", "")

		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	})

	// An archived locale is the state this branch introduced, and it is the one
	// negative answer OTA did not have. releasesvc.Publish and mergesvc
	// materialise bundles for ACTIVE locales only, so nothing ever refreshes an
	// archived locale's bundle — but the last one is still in release_bundles,
	// so before this check the endpoint answered 200 with it forever and every
	// client froze on stale copy instead of falling back to the strings shipped
	// in the binary. The stub returns a perfectly good bundle here precisely so
	// that a handler which forgot the status check would answer 200 and fail.
	t.Run("an archived locale is a cacheable 404", func(t *testing.T) {
		releases := &stubReleases{bundle: repository.ServableBundle{
			ReleaseVersion: 41, Strings: []byte(`{"a":"b"}`), SHA256: "abc",
		}}
		h := &Handler{
			locales: &stubLocales{byCode: map[string]model.Locale{
				"en-SG": {ID: 1, Code: "en-SG", Status: repository.LocaleArchived},
			}},
			releases: releases,
		}
		rec := otaRequest(h, "/ota/v1/bundles/en-SG", "")

		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
		assert.Contains(t, rec.Body.String(), "unknown_locale",
			"an archived locale answers exactly as an unknown one does — from a "+
				"client's point of view that is what it has become")
		assert.Empty(t, releases.appVersions,
			"an archived locale must not reach the bundle lookup at all")
	})

	t.Run("no release yet is a cacheable 404", func(t *testing.T) {
		releases := &stubReleases{err: repository.ErrNotFound}
		rec := otaRequest(otaHandler(releases), "/ota/v1/bundles/en-SG", "")

		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	})

	t.Run("the kill switch is a cacheable 410", func(t *testing.T) {
		releases := &stubReleases{
			bundle: repository.ServableBundle{KillSwitched: true},
			err:    repository.ErrNotFound,
		}
		rec := otaRequest(otaHandler(releases), "/ota/v1/bundles/en-SG", "")

		assert.Equal(t, http.StatusGone, rec.Code)
		assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	})
}

// TestOTABundleRejectsUnknownQueryParameters: the same rule as every other
// endpoint — a client that misspells a parameter must be told.
func TestOTABundleRejectsUnknownQueryParameters(t *testing.T) {
	releases := &stubReleases{bundle: repository.ServableBundle{
		ReleaseVersion: 41, Strings: []byte(`{}`), SHA256: "abc",
	}}
	rec := otaRequest(otaHandler(releases), "/ota/v1/bundles/en-SG?pretty=1", "")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown query parameter")
	assert.Empty(t, releases.appVersions, "a refused request must not reach the repository")
}

// TestMatchesETag covers the If-None-Match forms a real client or CDN sends.
//
// Comparing the raw header against our tag would fail every case below except
// the first, and the 304 path — the entire point of the OTA design — would
// silently never fire. The failure mode is invisible: everything still works,
// it just ships 60-80KB on every app launch instead of a few hundred bytes.
func TestMatchesETag(t *testing.T) {
	const etag = `"abc123"`

	cases := []struct {
		name, header string
		want         bool
	}{
		{"exact match", `"abc123"`, true},
		{"no header", "", false},
		{"different tag", `"def456"`, false},
		{"wildcard", "*", true},
		{"weak validator", `W/"abc123"`, true},
		{"multiple, ours last", `"x", "y", "abc123"`, true},
		{"multiple, ours absent", `"x", "y"`, false},
		{"multiple with spacing and weak", `W/"x" ,  W/"abc123"`, true},
		{"unquoted junk", `abc123`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesETag(tc.header, etag))
		})
	}
}

func TestSplitAndTrim(t *testing.T) {
	assert.Equal(t, []string{`"a"`, `"b"`}, splitAndTrim(`"a", "b"`, ','))
	assert.Equal(t, []string{`"a"`}, splitAndTrim(`  "a"  `, ','))
	assert.Empty(t, splitAndTrim(`  ,  `, ','))
}

func TestTrimWeakPrefix(t *testing.T) {
	assert.Equal(t, `"abc"`, trimWeakPrefix(`W/"abc"`))
	assert.Equal(t, `"abc"`, trimWeakPrefix(`"abc"`))
	// A tag that merely starts with W must not be mangled.
	assert.Equal(t, `"W123"`, trimWeakPrefix(`"W123"`))
}
