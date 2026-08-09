package route

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubPinger struct {
	err    error
	called bool
}

func (s *stubPinger) PingContext(context.Context) error {
	s.called = true
	return s.err
}

func TestHealthz_IsAliveAndDoesNotTouchTheDatabase(t *testing.T) {
	pinger := &stubPinger{err: errors.New("database is down")}
	h := &Handler{db: pinger}

	rec := httptest.NewRecorder()
	h.Healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body healthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, serviceName, body.Service)

	// The point of the liveness/readiness split: a dead database must not
	// make this endpoint fail, or Kubernetes restarts the whole fleet.
	assert.False(t, pinger.called, "liveness must not depend on the database")
}

func TestReadyz_ReadyWhenDatabaseReachable(t *testing.T) {
	pinger := &stubPinger{}
	h := &Handler{db: pinger}

	rec := httptest.NewRecorder()
	h.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body readinessResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ready", body.Status)
	assert.Equal(t, "ok", body.Checks["database"])
	assert.True(t, pinger.called, "readiness must actually probe the database")
}

func TestReadyz_UnavailableWhenDatabaseUnreachable(t *testing.T) {
	h := &Handler{db: &stubPinger{err: errors.New("connection refused")}}

	rec := httptest.NewRecorder()
	h.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body readinessResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "unavailable", body.Status)
	assert.Equal(t, "unreachable", body.Checks["database"])
}
