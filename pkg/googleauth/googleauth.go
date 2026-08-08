// Package googleauth turns an opaque Google OAuth access token into an email
// address, and nothing else.
//
// This is a deliberate half of what bo-api's AccessTokenFilter does. bo-api
// performs two steps: it validates the token against Google's tokeninfo
// endpoint to obtain an identity, and then queries the Google Admin Directory
// API to derive that identity's yp_* group permissions. Only the first step is
// ported here.
//
// The second step is not merely unnecessary, it would be wrong. u-l10n's roles
// live in its own `users` table keyed on email — a designer may be an l10n
// editor and nothing else, and a yp_* group says nothing about that. Deriving
// authorization from another system's groups would mean inheriting that
// system's every future change. So this package answers "who is this?" and
// pkg/repository.UserRepository answers "what may they do?".
//
// Skipping the Directory lookup is also what keeps this package small and
// deployable: no service account, no domain-wide delegation, no Directory API
// scope, no key to rotate.
package googleauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/config"
)

var log = ulog.GetLogger("u-l10n")

// defaultTokenInfoURL is the v1 endpoint bo-api uses. v1 rather than v3 because
// v1 answers for opaque access tokens; v3 is for ID tokens, which is a
// different credential the portal does not send.
const defaultTokenInfoURL = "https://www.googleapis.com/oauth2/v1/tokeninfo"

// maxCacheTTL caps how long a verification is reused regardless of how long the
// token itself has left.
//
// Caching at all is not optional: without it every portal request costs a round
// trip to Google, and a page that loads six panels pays for six. But a cache is
// also a revocation delay — a token withdrawn at Google keeps working here
// until its entry lapses. Five minutes is the compromise: it removes
// essentially all of the round trips (a Google access token lives an hour)
// while bounding how long a revoked credential outlives its revocation.
const maxCacheTTL = 5 * time.Minute

// pruneAt is the cache size that triggers a sweep of lapsed entries. The
// sweep is O(n) but runs only on a miss, and only once the map is large enough
// for the walk to be worth it.
const pruneAt = 1024

// ErrInvalidToken means Google refused the token, or accepted it and returned
// no email. It is an exported sentinel so the middleware can map it to 401 with
// errors.Is, and distinguish it from Google being unreachable — which is a 5xx
// and must NOT send a valid operator away to re-authenticate.
var ErrInvalidToken = errors.New("access token is not valid")

// TokenInfo is the useful subset of Google's tokeninfo response.
type TokenInfo struct {
	// Email is the whole point of this package.
	Email string
	// UserID is Google's stable subject identifier. Carried for audit metadata:
	// an email can be reassigned, this cannot.
	UserID string
	// ExpiresIn is the token's remaining lifetime in seconds, as reported by
	// Google at the moment of verification.
	ExpiresIn int64
	// Audience is the OAuth client the token was issued to. See Verifier.audience.
	Audience string
}

// tokenInfoResponse mirrors the wire format. Separate from TokenInfo so the
// JSON tag names, which are Google's to change, do not leak into the rest of
// the service.
type tokenInfoResponse struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	ExpiresIn int64  `json:"expires_in"`
	Audience  string `json:"audience"`
	Scope     string `json:"scope"`
}

// Verifier validates access tokens against Google, with an in-process cache.
type Verifier struct {
	url      string
	audience string
	http     *http.Client
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	info     TokenInfo
	expireAt time.Time
}

// Option configures a Verifier. The two that exist are what the tests need to
// run without a network — the same reason assetsvc declares its own ObjectStore
// and route declares its own Pinger.
type Option func(*Verifier)

func WithTokenInfoURL(u string) Option     { return func(v *Verifier) { v.url = u } }
func WithHTTPClient(h *http.Client) Option { return func(v *Verifier) { v.http = h } }

// withClock lets a test move time forward without sleeping. Unexported: the
// production graph has exactly one clock.
func withClock(now func() time.Time) Option { return func(v *Verifier) { v.now = now } }

// New builds a Verifier.
//
// audience, when non-empty, is the OAuth client id the portal authenticates
// with; a token issued to any other client is refused. See Verify.
func New(audience string, opts ...Option) *Verifier {
	v := &Verifier{
		url:      defaultTokenInfoURL,
		audience: audience,
		// A timeout, because this call sits on the critical path of every
		// portal request. Without one a hung connection to Google holds a
		// request slot until the server's own timeout fires.
		http:  &http.Client{Timeout: 10 * time.Second},
		now:   time.Now,
		cache: make(map[string]entry),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// ProvideVerifier builds the Verifier for the Wire graph.
func ProvideVerifier(cnf *config.Config) *Verifier {
	if cnf.GoogleOAuthAudience == "" {
		// Loud, once, at startup rather than per request. See Verify for what
		// is being given up.
		log.Infow(context.Background(),
			"google oauth audience is not configured; any Google access token for a "+
				"provisioned email will be accepted regardless of which OAuth client issued it",
			"config", "SERVICECONFIG_GOOGLE_OAUTH_AUDIENCE")
	}
	return New(cnf.GoogleOAuthAudience)
}

// Verify resolves an access token to an identity, from cache when possible.
//
// Returns ErrInvalidToken when Google rejects the token or returns no email.
// Any other error means the check could not be made — Google unreachable, a 5xx
// from Google, an unreadable body — and the caller must not treat that as a
// failed authentication.
func (v *Verifier) Verify(ctx context.Context, accessToken string) (TokenInfo, error) {
	if accessToken == "" {
		return TokenInfo{}, ErrInvalidToken
	}

	// The cache is keyed on the SHA-256 of the token, not the token. The map
	// outlives the request by design, and a long-lived in-process map of live
	// bearer credentials is a thing worth not having — in a heap dump, in a
	// core file, or in a debugger.
	key := cacheKey(accessToken)

	if info, ok := v.lookup(key); ok {
		return info, nil
	}

	info, err := v.fetch(ctx, accessToken)
	if err != nil {
		return TokenInfo{}, err
	}

	v.store(key, info)
	return info, nil
}

func (v *Verifier) lookup(key string) (TokenInfo, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	e, ok := v.cache[key]
	if !ok || !v.now().Before(e.expireAt) {
		return TokenInfo{}, false
	}
	return e.info, true
}

// store caches a verification for the token's own remaining lifetime, capped at
// maxCacheTTL.
//
// Taking the minimum is what stops the cache from extending a token's life: a
// token with four seconds left is cached for four seconds, not for five
// minutes, so the next request after it lapses asks Google and is refused.
func (v *Verifier) store(key string, info TokenInfo) {
	ttl := time.Duration(info.ExpiresIn) * time.Second
	if ttl > maxCacheTTL {
		ttl = maxCacheTTL
	}
	if ttl <= 0 {
		// Nothing worth caching, and caching it would be a lie about the
		// token's validity.
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if len(v.cache) >= pruneAt {
		now := v.now()
		for k, e := range v.cache {
			if !now.Before(e.expireAt) {
				delete(v.cache, k)
			}
		}
	}

	v.cache[key] = entry{info: info, expireAt: v.now().Add(ttl)}
}

// fetch asks Google.
func (v *Verifier) fetch(ctx context.Context, accessToken string) (TokenInfo, error) {
	// The token travels as a query parameter because that is the endpoint's
	// only interface. It is therefore never logged and the URL is never
	// included in an error — an error string is as good as a log line for
	// leaking a credential.
	endpoint := v.url + "?access_token=" + url.QueryEscape(accessToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("build tokeninfo request: %w", err)
	}

	rsp, err := v.http.Do(req)
	if err != nil {
		// Deliberately NOT ErrInvalidToken: Google being unreachable says
		// nothing about the token, and answering 401 would tell every operator
		// in the building to sign in again during someone else's outage.
		return TokenInfo{}, fmt.Errorf("call google tokeninfo: %w", redactToken(err, accessToken))
	}
	defer func() { _ = rsp.Body.Close() }()

	// A bounded read: this is a small JSON document, and an unbounded ReadAll
	// against a remote host is an unbounded allocation.
	body, err := io.ReadAll(io.LimitReader(rsp.Body, 64<<10))
	if err != nil {
		return TokenInfo{}, fmt.Errorf("read tokeninfo response: %w", err)
	}

	switch {
	case rsp.StatusCode == http.StatusOK:
	case rsp.StatusCode == http.StatusBadRequest, rsp.StatusCode == http.StatusUnauthorized:
		// This is what an expired, revoked or fabricated token looks like:
		// 400 with {"error":"invalid_token"}.
		return TokenInfo{}, ErrInvalidToken
	default:
		// 429, 500, 503 — Google's problem, not the caller's.
		return TokenInfo{}, fmt.Errorf("google tokeninfo returned %d", rsp.StatusCode)
	}

	var parsed tokenInfoResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return TokenInfo{}, fmt.Errorf("decode tokeninfo response: %w", err)
	}

	// A 200 with no email is not an identity. bo-api treats this as a refusal
	// too, and it is the only check standing between us and a token issued for
	// a scope that carries no email at all.
	if parsed.Email == "" {
		return TokenInfo{}, ErrInvalidToken
	}
	if parsed.ExpiresIn <= 0 {
		return TokenInfo{}, ErrInvalidToken
	}

	// Audience, when configured.
	//
	// Any Google OAuth client can mint an access token for a you.co user, and
	// tokeninfo will happily validate it. Without this check, a token obtained
	// by an unrelated application — one the operator signed into for some other
	// purpose — is accepted here as proof of intent to use u-l10n. That is the
	// classic confused-deputy shape. Pinning the audience to the portal's own
	// client id closes it.
	//
	// It is enforced only when configured, so that an environment which has not
	// yet been told the client id behaves as bo-api does today rather than
	// refusing every operator. ProvideVerifier says so at startup.
	if v.audience != "" && parsed.Audience != v.audience {
		log.Infow(ctx, "rejected access token: wrong audience",
			"email", parsed.Email, "audience", parsed.Audience)
		return TokenInfo{}, ErrInvalidToken
	}

	return TokenInfo{
		Email:     parsed.Email,
		UserID:    parsed.UserID,
		ExpiresIn: parsed.ExpiresIn,
		Audience:  parsed.Audience,
	}, nil
}

func cacheKey(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:])
}

// redactToken removes the access token from an error before it can be returned,
// logged or rendered.
//
// net/http embeds the request URL in transport errors, and the URL carries the
// token. Nothing else in this package puts the token in a string, but this one
// path does it on our behalf.
func redactToken(err error, accessToken string) error {
	if accessToken == "" {
		return err
	}
	text := err.Error()
	for _, form := range []string{url.QueryEscape(accessToken), accessToken} {
		text = strings.ReplaceAll(text, form, "REDACTED")
	}
	if text == err.Error() {
		return err
	}
	return errors.New(text)
}
