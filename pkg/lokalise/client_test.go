package lokalise

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No Lokalise token exists yet (open item #4), so these run against a fake
// server. That is not a limitation: the behaviour worth testing is how the
// client reacts to the API's edges — pagination, throttling, failure — and a
// fake reproduces those on demand where the real API would not.

func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	opts = append([]Option{WithBaseURL(srv.URL), WithRatePerSecond(1000)}, opts...)
	c := New("test-token", "proj-1", opts...)
	t.Cleanup(c.Close)
	return c
}

func TestKeysFollowsPagination(t *testing.T) {
	var pages int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&pages, 1)
		assert.Equal(t, "test-token", r.Header.Get("X-Api-Token"))

		// Two full pages of 500, then a short one — the short page is what
		// signals the end.
		count := 500
		if page == 3 {
			count = 7
		}
		w.Write([]byte(keysPayload(int(page), count)))
	}))

	keys, err := c.Keys(context.Background())
	require.NoError(t, err)

	assert.Len(t, keys, 1007, "all pages must be collected")
	assert.EqualValues(t, 3, atomic.LoadInt32(&pages), "a short page ends pagination")
}

func TestRetriesOnTooManyRequestsAndHonoursRetryAfter(t *testing.T) {
	var calls int32
	var gap time.Duration
	var firstAt time.Time

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			firstAt = time.Now()
			// Lokalise sends seconds; 1 is the smallest value that is still
			// observable without making the test slow.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		gap = time.Since(firstAt)
		w.Write([]byte(`{"keys":[]}`))
	}))

	_, err := c.Keys(context.Background())
	require.NoError(t, err, "a 429 must be retried, not surfaced")

	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
	assert.GreaterOrEqual(t, gap, time.Second,
		"the server's Retry-After must be respected, not replaced with our own guess")
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad token"}}`))
	}))

	_, err := c.Keys(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls),
		"a 401 will not improve with retrying; hammering the API is the wrong response to a wrong token")
}

func TestRetriesServerErrorsThenGivesUp(t *testing.T) {
	var calls int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))

	_, err := c.Keys(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gave up")
	assert.EqualValues(t, maxAttempts, atomic.LoadInt32(&calls),
		"retries must be bounded so a permanent failure surfaces instead of looping")
}

// TestRateLimiterThrottles proves the client self-limits. Being throttled
// mid-import is worse than taking an extra thirty seconds.
func TestRateLimiterThrottles(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"keys":[]}`))
	}), WithRatePerSecond(20)) // 50ms apart

	start := time.Now()
	for i := 0; i < 4; i++ {
		_, err := c.Keys(context.Background())
		require.NoError(t, err)
	}
	elapsed := time.Since(start)

	// Four requests at 20/s cannot complete faster than ~150ms.
	assert.GreaterOrEqual(t, elapsed, 140*time.Millisecond,
		"requests completed too fast; the rate limiter is not throttling")
}

func TestContextCancellationIsHonoured(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Keys(ctx)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second,
		"a cancelled context must abort the backoff wait rather than sleeping out a 30s Retry-After")
}

// TestKeyNamePrefersWeb pins the per-platform name rule. 'web' maps to flutter
// and carries the UNTRANSFORMED name; Android's is transformed and lossy, so it
// is the last resort.
func TestKeyNamePrefersWeb(t *testing.T) {
	cases := []struct {
		name KeyName
		want string
	}{
		{KeyName{Web: "01-menu-help", Android: "_menu_help", IOS: "01-menu-help"}, "01-menu-help"},
		{KeyName{IOS: "ios-only-key"}, "ios-only-key"},
		{KeyName{Android: "android_only"}, "android_only"},
		{KeyName{}, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.name.Canonical())
	}
}

// TestTranslationsCannotDistinguishEmptyFromAbsent documents the API limitation
// that forces the importer to consult the file exports.
func TestTranslationsCannotDistinguishEmptyFromAbsent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"keys":[{"key_id":1,"key_name":{"web":"k"},
			"translations":[{"language_iso":"en-SG","translation":""}]}]}`))
	}))

	keys, err := c.Keys(context.Background())
	require.NoError(t, err)
	require.Len(t, keys, 1)

	assert.Equal(t, "", keys[0].Translations[0].Value,
		"the API returns \"\" for BOTH deliberately-blank and untranslated; "+
			"only the file exports can tell them apart, which is why the importer needs them")
}

func keysPayload(page, count int) string {
	out := `{"keys":[`
	for i := 0; i < count; i++ {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"key_id":%d,"key_name":{"web":"key_%d_%d"},"platforms":["web"]}`,
			page*1000+i, page, i)
	}
	return out + `]}`
}
