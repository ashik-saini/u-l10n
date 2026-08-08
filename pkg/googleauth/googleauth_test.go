package googleauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGoogle stands in for the tokeninfo endpoint.
//
// Every test here drives the refusal paths, which is the whole reason this
// package talks to an interface-shaped seam rather than to net/http directly:
// a verifier whose rejections can only be exercised with a live Google token is
// a verifier whose rejections are never exercised.
type fakeGoogle struct {
	mu sync.Mutex
	// respond returns the status and body for the presented token.
	respond func(token string) (int, string)
	calls   int32
	// seen records the tokens the endpoint was asked about.
	seen []string
}

func (f *fakeGoogle) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls, 1)
		token := r.URL.Query().Get("access_token")

		f.mu.Lock()
		f.seen = append(f.seen, token)
		respond := f.respond
		f.mu.Unlock()

		status, body := respond(token)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGoogle) count() int { return int(atomic.LoadInt32(&f.calls)) }

func okBody(email string, expiresIn int64) string {
	return fmt.Sprintf(
		`{"issued_to":"c.apps.googleusercontent.com","audience":"portal-client",
		  "user_id":"1029","expires_in":%d,"email":%q,"verified_email":true}`,
		expiresIn, email)
}

// newTestVerifier wires a Verifier to the fake, with a controllable clock.
func newTestVerifier(t *testing.T, srv *httptest.Server, audience string, now func() time.Time) *Verifier {
	t.Helper()
	return New(audience,
		WithTokenInfoURL(srv.URL),
		WithHTTPClient(srv.Client()),
		withClock(now))
}

func TestVerifyReturnsTheIdentity(t *testing.T) {
	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("ashik.saini@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	info, err := v.Verify(context.Background(), "good-token")
	require.NoError(t, err)
	assert.Equal(t, "ashik.saini@you.co", info.Email)
	assert.Equal(t, "1029", info.UserID)
	assert.Equal(t, int64(3600), info.ExpiresIn)
}

// TestVerifyRefusals is the important table. Every row is a way in which a
// caller must NOT be admitted, or must not be turned away.
func TestVerifyRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		token   string
		invalid bool // errors.Is(err, ErrInvalidToken)
	}{
		{
			// What an expired, revoked or fabricated token actually looks like.
			name: "google rejects the token", status: http.StatusBadRequest,
			body: `{"error":"invalid_token"}`, token: "garbage", invalid: true,
		},
		{
			name: "google answers 401", status: http.StatusUnauthorized,
			body: `{"error":"unauthorized"}`, token: "stale", invalid: true,
		},
		{
			// A 200 with no email is not an identity.
			name: "token is valid but carries no email", status: http.StatusOK,
			body: `{"user_id":"1029","expires_in":3600}`, token: "scopeless", invalid: true,
		},
		{
			// Already expired at the moment we asked.
			name: "token has no life left", status: http.StatusOK,
			body: okBody("a@you.co", 0), token: "expired", invalid: true,
		},
		{
			// Google's problem, not the caller's. Answering 401 here would tell
			// every operator in the building to sign in again during someone
			// else's outage.
			name: "google is broken", status: http.StatusInternalServerError,
			body: `{"error":"backend"}`, token: "fine", invalid: false,
		},
		{
			name: "google is rate limiting us", status: http.StatusTooManyRequests,
			body: `{"error":"rate"}`, token: "fine", invalid: false,
		},
		{
			name: "response is not json", status: http.StatusOK,
			body: `<html>proxy error</html>`, token: "fine", invalid: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			google := &fakeGoogle{respond: func(string) (int, string) {
				return tc.status, tc.body
			}}
			v := newTestVerifier(t, google.start(t), "", time.Now)

			_, err := v.Verify(context.Background(), tc.token)
			require.Error(t, err)
			assert.Equal(t, tc.invalid, errors.Is(err, ErrInvalidToken),
				"an unreachable or broken Google must not be reported as a bad token, "+
					"and a bad token must not be reported as an outage")
		})
	}
}

// TestVerifyRefusesAnEmptyTokenWithoutAskingGoogle: an empty Authorization
// header is a client bug, and spending a round trip on it is how a login page
// refresh loop becomes a Google quota incident.
func TestVerifyRefusesAnEmptyTokenWithoutAskingGoogle(t *testing.T) {
	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	_, err := v.Verify(context.Background(), "")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Equal(t, 0, google.count())
}

// TestVerifyEnforcesAudienceWhenConfigured.
//
// Any Google OAuth client can mint an access token for a you.co user and
// tokeninfo will validate it. Without this check a token obtained by an
// unrelated application is accepted here as proof of intent to use u-l10n.
func TestVerifyEnforcesAudienceWhenConfigured(t *testing.T) {
	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	srv := google.start(t)

	t.Run("wrong audience is refused", func(t *testing.T) {
		v := newTestVerifier(t, srv, "some-other-client", time.Now)
		_, err := v.Verify(context.Background(), "token")
		assert.ErrorIs(t, err, ErrInvalidToken)
	})

	t.Run("matching audience is admitted", func(t *testing.T) {
		v := newTestVerifier(t, srv, "portal-client", time.Now)
		info, err := v.Verify(context.Background(), "token")
		require.NoError(t, err)
		assert.Equal(t, "a@you.co", info.Email)
	})

	t.Run("unconfigured audience admits any client", func(t *testing.T) {
		v := newTestVerifier(t, srv, "", time.Now)
		_, err := v.Verify(context.Background(), "token")
		assert.NoError(t, err)
	})
}

// TestCacheAvoidsTheRoundTrip. Without this every portal request costs a call
// to Google, and a page that loads six panels pays six times.
func TestCacheAvoidsTheRoundTrip(t *testing.T) {
	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	for i := 0; i < 5; i++ {
		_, err := v.Verify(context.Background(), "token")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, google.count())

	// A different token is a different entry, not a cache hit.
	google.mu.Lock()
	google.respond = func(string) (int, string) { return http.StatusOK, okBody("b@you.co", 3600) }
	google.mu.Unlock()

	info, err := v.Verify(context.Background(), "other-token")
	require.NoError(t, err)
	assert.Equal(t, "b@you.co", info.Email)
	assert.Equal(t, 2, google.count())
}

// TestCacheDoesNotExtendATokenBeyondItsTTL is the refusal that matters most in
// this package: a cache that outlives the credential it caches has quietly
// granted an extension nobody authorised.
func TestCacheDoesNotExtendATokenBeyondItsTTL(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	// A token with two seconds left, which Google then rejects.
	var expired atomic.Bool
	google := &fakeGoogle{respond: func(string) (int, string) {
		if expired.Load() {
			return http.StatusBadRequest, `{"error":"invalid_token"}`
		}
		return http.StatusOK, okBody("a@you.co", 2)
	}}
	v := newTestVerifier(t, google.start(t), "", clock)

	_, err := v.Verify(context.Background(), "short-lived")
	require.NoError(t, err)

	// Still inside its life: served from cache, no second call.
	advance(time.Second)
	_, err = v.Verify(context.Background(), "short-lived")
	require.NoError(t, err)
	assert.Equal(t, 1, google.count())

	// Past its life. The cache must not answer, and Google's refusal must reach
	// the caller.
	advance(2 * time.Second)
	expired.Store(true)
	_, err = v.Verify(context.Background(), "short-lived")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Equal(t, 2, google.count())
}

// TestCacheIsCappedBelowTheTokenLifetime: a Google access token lives an hour,
// but caching a verification for an hour means a revoked token keeps working
// for an hour. The cap bounds that.
func TestCacheIsCappedBelowTheTokenLifetime(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", clock)

	_, err := v.Verify(context.Background(), "hour-long")
	require.NoError(t, err)

	mu.Lock()
	now = now.Add(maxCacheTTL + time.Second)
	mu.Unlock()

	_, err = v.Verify(context.Background(), "hour-long")
	require.NoError(t, err)
	assert.Equal(t, 2, google.count(),
		"the cap must force a re-check well inside the token's own hour")
}

// TestFailuresAreNotCached: a token refused once must be asked about again, and
// a Google outage must not poison the cache for the tokens it touched.
func TestFailuresAreNotCached(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	google := &fakeGoogle{respond: func(string) (int, string) {
		if fail.Load() {
			return http.StatusInternalServerError, `{"error":"backend"}`
		}
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	_, err := v.Verify(context.Background(), "token")
	require.Error(t, err)

	fail.Store(false)
	info, err := v.Verify(context.Background(), "token")
	require.NoError(t, err)
	assert.Equal(t, "a@you.co", info.Email)
	assert.Equal(t, 2, google.count())
}

// TestTheTokenNeverAppearsInAnError.
//
// net/http embeds the request URL in transport errors, and the token rides in
// that URL. An error string is as good as a log line for leaking a credential.
func TestTheTokenNeverAppearsInAnError(t *testing.T) {
	const secret = "ya29.super-secret-access-token"

	// A server that is closed immediately, so Do fails at the transport layer
	// with the URL in the message.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := srv.Client()
	url := srv.URL
	srv.Close()

	v := New("", WithTokenInfoURL(url), WithHTTPClient(client))

	_, err := v.Verify(context.Background(), secret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "REDACTED")
}

// TestCacheIsKeyedOnAHashNotTheToken: the map outlives the request, and a
// long-lived in-process map of live bearer credentials is a thing worth not
// having — in a heap dump, a core file, or a debugger.
func TestCacheIsKeyedOnAHashNotTheToken(t *testing.T) {
	const secret = "ya29.another-secret"
	google := &fakeGoogle{respond: func(string) (int, string) {
		return http.StatusOK, okBody("a@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	_, err := v.Verify(context.Background(), secret)
	require.NoError(t, err)

	v.mu.Lock()
	defer v.mu.Unlock()
	require.Len(t, v.cache, 1)
	for k := range v.cache {
		assert.NotContains(t, k, secret)
		assert.Len(t, k, 64, "a hex sha256")
	}
}

// TestConcurrentVerifyIsSafe. The middleware is on every portal request, so the
// cache is touched by every request handler goroutine at once. Run with -race.
func TestConcurrentVerifyIsSafe(t *testing.T) {
	google := &fakeGoogle{respond: func(token string) (int, string) {
		return http.StatusOK, okBody(token+"@you.co", 3600)
	}}
	v := newTestVerifier(t, google.start(t), "", time.Now)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("token%d", i%5)
			info, err := v.Verify(context.Background(), token)
			assert.NoError(t, err)
			assert.True(t, strings.HasPrefix(info.Email, token))
		}(i)
	}
	wg.Wait()
}
