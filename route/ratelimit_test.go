package route

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/apm"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/googleauth"
)

// fakeClock lets the tests move time forward without sleeping — the same
// pattern googleauth's cache tests use.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(perMinute, burst int) (*ipRateLimiter, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newIPRateLimiter(&config.Config{
		RateLimitEnable:    true,
		RateLimitPerMinute: perMinute,
		RateLimitBurst:     burst,
	}, clock.now)
	return l, clock
}

func hit(l *ipRateLimiter, ip string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
	req.RemoteAddr = ip + ":54321"
	l.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	return rec
}

func TestLimiterAllowsTheBurstThenRefuses(t *testing.T) {
	l, _ := newTestLimiter(60, 5)

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code, "request %d is within the burst", i+1)
	}

	rec := hit(l, "10.0.0.1")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	// The refusal must carry the same contract this service's own Lokalise
	// client relies on from the other side: a Retry-After a client can obey.
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	require.NoError(t, err, "Retry-After must be present and numeric")
	assert.GreaterOrEqual(t, retryAfter, 1)
	assert.Contains(t, rec.Body.String(), "rate_limited")
}

func TestLimiterRefillsWithTime(t *testing.T) {
	l, clock := newTestLimiter(60, 1) // one token, one per second

	require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code)
	require.Equal(t, http.StatusTooManyRequests, hit(l, "10.0.0.1").Code)

	clock.advance(1100 * time.Millisecond)
	assert.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code,
		"a second's refill at 60/min must grant exactly one more request")
	assert.Equal(t, http.StatusTooManyRequests, hit(l, "10.0.0.1").Code,
		"and only one — refill must not exceed elapsed time")
}

func TestLimiterRefillNeverExceedsBurst(t *testing.T) {
	l, clock := newTestLimiter(600, 3)

	// A long quiet period must not bank more than the burst.
	clock.advance(time.Hour)
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, hit(l, "10.0.0.1").Code,
		"an hour idle at 600/min must still cap at the 3-token burst")
}

// TestLimiterIsolatesClients: one client's flood must not spend another's
// budget — the entire reason the key is per-IP rather than global.
func TestLimiterIsolatesClients(t *testing.T) {
	l, _ := newTestLimiter(60, 2)

	require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code)
	require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code)
	require.Equal(t, http.StatusTooManyRequests, hit(l, "10.0.0.1").Code)

	assert.Equal(t, http.StatusOK, hit(l, "10.0.0.2").Code,
		"an exhausted neighbour must not starve a fresh client")
}

func TestLimiterDisabledIsAPassthrough(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newIPRateLimiter(&config.Config{RateLimitEnable: false}, clock.now)

	for i := 0; i < 50; i++ {
		require.Equal(t, http.StatusOK, hit(l, "10.0.0.1").Code)
	}
}

// TestLimiterPrunesFullBuckets guards the memory bound: an address-spraying
// client must not grow the map without limit, and pruning must only ever drop
// buckets that carry no state a fresh one would not.
func TestLimiterPrunesFullBuckets(t *testing.T) {
	l, clock := newTestLimiter(600, 2)

	// One client left mid-budget: it must SURVIVE the prune.
	require.Equal(t, http.StatusOK, hit(l, "10.9.9.9").Code)

	// Fill the map past the prune threshold with distinct clients.
	for i := 0; i < bucketPruneAt; i++ {
		hit(l, "10.1."+strconv.Itoa(i/250)+"."+strconv.Itoa(i%250))
	}

	// Long enough for every bucket to refill to full — except that none of the
	// sprayed ones have been touched since, so all become prunable.
	clock.advance(time.Minute)

	// The next new client triggers the sweep.
	require.Equal(t, http.StatusOK, hit(l, "10.2.0.1").Code)

	l.mu.Lock()
	size := len(l.buckets)
	l.mu.Unlock()
	assert.Less(t, size, 10,
		"the sweep must drop refilled buckets; %d survivors means it is not bounding memory", size)
}

// TestRateLimitMountedBeforeAuth is the property that makes the limiter worth
// having: a flood of garbage credentials must be refused BEFORE the expensive
// pre-auth work — the outbound Google call, the token hash lookup — not after.
// Driven through the real router, nil services and all, so the mounting order
// in ProvideRoutes is what is under test.
func TestRateLimitMountedBeforeAuth(t *testing.T) {
	// Same construction as TestPortalRoutesRequireAnIdentity: the real router,
	// doubles for the identity path, nil for everything else so an unexpected
	// reach into a service panics rather than quietly passing.
	handler := identityHandler(&stubVerifier{err: googleauth.ErrInvalidToken}, &stubUsers{})
	handler.cnf = &config.Config{
		RequestTimeout:     time.Minute,
		RateLimitEnable:    true,
		RateLimitPerMinute: 60,
		RateLimitBurst:     2,
	}
	router := ProvideRoutes(&apm.ApmConfig{}, handler.cnf, handler)

	status := func(path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.3.0.1:1000"
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// Unauthenticated requests: the first two spend the burst on 401s...
	require.Equal(t, http.StatusUnauthorized, status("/api/v1/me"))
	require.Equal(t, http.StatusUnauthorized, status("/api/v1/me"))
	// ...and the third is refused by the LIMITER, proving it runs first.
	assert.Equal(t, http.StatusTooManyRequests, status("/api/v1/me"))

	// The probes are mounted OUTSIDE the governed groups: a kubelet that gets
	// 429 from a liveness check restarts a healthy pod under load, which turns
	// an overload into an outage.
	for i := 0; i < 10; i++ {
		assert.Equal(t, http.StatusOK, status("/healthz"), "probe %d must be exempt", i)
	}
}
