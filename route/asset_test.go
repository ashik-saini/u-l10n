package route

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi"
	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/apm"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/assetsvc"
)

// TestAssetErrorStatusCodes pins the sentinel-to-status mapping.
//
// Getting this wrong is quiet and expensive in both directions: a caller error
// returned as 500 sends an on-call engineer after a fault that does not exist,
// and an infrastructure failure returned as 4xx hides a real outage behind what
// looks like a client bug.
func TestAssetErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"bad declaration", fmt.Errorf("%w: bytes too large", assetsvc.ErrBadRequest), http.StatusBadRequest, "bad_request"},
		{"unknown asset", fmt.Errorf("asset 9: %w", repository.ErrNotFound), http.StatusNotFound, "not_found"},
		{"object never uploaded", fmt.Errorf("%w (sha256 x)", assetsvc.ErrUploadNotFound), http.StatusConflict, "upload_not_found"},
		{"size mismatch", fmt.Errorf("%w: declared 1", assetsvc.ErrUploadMismatch), http.StatusConflict, "upload_mismatch"},
		{"no declaration", fmt.Errorf("%w", assetsvc.ErrUploadUnverifiable), http.StatusConflict, "upload_mismatch"},
		{"s3 unreachable", errors.New("dial tcp: connection refused"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/assets/confirm", nil)

			h.assetError(w, r, "confirm", tc.err)

			assert.Equal(t, tc.want, w.Code)
			assert.Contains(t, w.Body.String(), tc.code)
		})
	}
}

// TestAssetErrorDoesNotLeakInternalDetail: a 500 says nothing about why. The
// reason is in the log, where it belongs.
func TestAssetErrorDoesNotLeakInternalDetail(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/assets/confirm", nil)

	h.assetError(w, r, "confirm", errors.New("host=db.internal user=l10n password=hunter2"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "hunter2")
	assert.NotContains(t, w.Body.String(), "db.internal")
}

func TestDecodeJSON(t *testing.T) {
	t.Run("accepts a well-formed body", func(t *testing.T) {
		var got presignRequest
		err := decodeInto(t, `{"filename":"a.png","content_type":"image/png","bytes":10,"sha256":"ab"}`, &got)
		require.NoError(t, err)
		assert.Equal(t, "a.png", got.Filename)
		assert.Equal(t, 10, got.Bytes)
	})

	// A misspelled field must be an error, not a silently defaulted zero. The
	// same reasoning as parseExportRequest rejecting unknown query parameters:
	// a client handed a default it did not ask for debugs the wrong layer.
	t.Run("rejects an unknown field", func(t *testing.T) {
		var got presignRequest
		err := decodeInto(t, `{"filename":"a.png","contentType":"image/png"}`, &got)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contentType")
	})

	t.Run("rejects trailing content", func(t *testing.T) {
		var got confirmRequest
		err := decodeInto(t, `{"sha256":"ab"}{"sha256":"cd"}`, &got)
		require.Error(t, err)
	})

	t.Run("rejects an empty body", func(t *testing.T) {
		var got confirmRequest
		err := decodeInto(t, ``, &got)
		require.Error(t, err)
	})

	// The image bytes never pass through this process; a body this size on
	// these endpoints is either a mistake or an attempt to make us allocate.
	t.Run("rejects an oversized body", func(t *testing.T) {
		var got confirmRequest
		padding := strings.Repeat("a", maxAssetBodyBytes+1)
		err := decodeInto(t, `{"sha256":"`+padding+`"}`, &got)
		require.Error(t, err)
	})
}

func decodeInto(t *testing.T, body string, dst any) error {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return decodeJSON(w, r, dst)
}

func TestPathID(t *testing.T) {
	cases := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"42", 42, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
		{"", 0, true},
		// A float or a padded value is a client that built the URL wrongly.
		{"1.5", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.raw)
			r := httptest.NewRequest(http.MethodGet, "/", nil).
				WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))

			got, err := pathID(r, "id")
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// stubTokens is an APITokenRepository that refuses everything, which is all
// the gating test needs.
type stubTokens struct{ called int }

func (s *stubTokens) Authenticate(context.Context, *gorm.DB, string) (repository.APIToken, error) {
	s.called++
	return repository.APIToken{}, repository.ErrNotFound
}

func (s *stubTokens) Create(context.Context, *gorm.DB, string, string, string, *time.Time) (string, repository.APIToken, error) {
	return "", repository.APIToken{}, nil
}

func (s *stubTokens) Revoke(context.Context, *gorm.DB, string, string) error { return nil }

// TestAssetRoutesRequireAToken walks the real router.
//
// These endpoints hand out credentials to screenshots full of customer names,
// balances and card numbers. A route accidentally mounted outside the
// authenticated group would be invisible in a unit test of the handler itself,
// which is exactly why this drives ProvideRoutes rather than the methods.
//
// The asset service is deliberately nil: if any of these requests reached a
// handler, the test would panic rather than quietly pass.
func TestAssetRoutesRequireAToken(t *testing.T) {
	tokens := &stubTokens{}
	handler := &Handler{
		cnf:    &config.Config{RequestTimeout: time.Second},
		tokens: tokens,
	}
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/assets/presign"},
		{http.MethodPost, "/api/v1/assets/confirm"},
		{http.MethodGet, "/api/v1/assets/5/url"},
		{http.MethodPut, "/api/v1/keys/1/assets"},
		{http.MethodDelete, "/api/v1/keys/1/assets/5"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "no token at all")

			rec = httptest.NewRecorder()
			r := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
			r.Header.Set("X-Api-Token", "ul10n_bogus")
			router.ServeHTTP(rec, r)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "unknown token")
		})
	}

	assert.Equal(t, len(routes), tokens.called, "every route must reach the token check")
}

// TestAssetHandlersRejectUnknownQueryParameters.
//
// None of these routes takes a query parameter, so any at all is a caller
// mistake to report. The Handler carries no services: the refusal must happen
// before anything else, or the test panics rather than quietly passing.
func TestAssetHandlersRejectUnknownQueryParameters(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		name   string
		method string
		target string
		call   func(http.ResponseWriter, *http.Request)
	}{
		{"presign", http.MethodPost, "/api/v1/assets/presign?dedupe=1", h.AssetPresign},
		{"confirm", http.MethodPost, "/api/v1/assets/confirm?wait=true", h.AssetConfirm},
		{"url", http.MethodGet, "/api/v1/assets/5/url?ttl=60", h.AssetURL},
		{"attach", http.MethodPut, "/api/v1/keys/1/assets?note=x", h.AssetAttach},
		{"detach", http.MethodDelete, "/api/v1/keys/1/assets/5?force=1", h.AssetDetach},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, httptest.NewRequest(tc.method, tc.target, strings.NewReader("{}")))

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "unknown query parameter")
		})
	}
}

// TestActorFromContext: uploaded_by and audit_events.actor are NOT NULL, and
// an audit trail whose actor is blank answers nothing.
func TestActorFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyToken{},
		repository.APIToken{Name: "ci", Scope: repository.ScopeReadWrite})
	assert.Equal(t, "token:ci", actorFromContext(ctx))

	assert.Equal(t, "unauthenticated", actorFromContext(context.Background()))
}
