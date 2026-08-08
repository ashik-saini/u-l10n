package route

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/config"
)

// ipRateLimiter is an in-process, per-client-IP token bucket.
//
// Why not the house filter. u-web-util's RateLimitFilter is deprecated by its
// own comment, is backed by Redis — which this service does not have — and is
// keyed on ctxutil.UserID, answering 400 whenever no user is in context. The
// surfaces that need protecting here are precisely the ones with no user in
// context: /ota/v1 is unauthenticated by design, and the identity middleware
// makes an outbound Google call with a 10-second timeout BEFORE any identity
// exists. The house filter cannot sit in front of either; this can.
//
// Why in-process is enough. u-l10n is a single global instance by decision
// (guide ch.16), so there is no fleet to coordinate a budget across. With N
// replicas the effective budget is N × the configured one — acceptable for
// what this is: a resource-exhaustion backstop, not a billing meter. The
// gateway and CDN in front remain the first line; this is the line that exists
// even when a caller reaches the service directly.
//
// The client key is the IP that middleware.RealIP resolved, which trusts
// X-Forwarded-For. Behind Istio that header is set by the mesh; if this
// service were ever exposed without a trusted proxy, RealIP — not this
// limiter — is the thing that would need revisiting.
type ipRateLimiter struct {
	enabled   bool
	perSecond float64
	burst     float64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// bucketPruneAt is the tracked-client count that triggers a sweep of full,
// idle buckets — the same on-miss pruning shape as googleauth's token cache,
// so the map cannot grow without bound under an address-spraying client.
const bucketPruneAt = 4096

func newIPRateLimiter(cnf *config.Config, now func() time.Time) *ipRateLimiter {
	return &ipRateLimiter{
		enabled:   cnf.RateLimitEnable,
		perSecond: float64(cnf.RateLimitPerMinute) / 60.0,
		burst:     float64(cnf.RateLimitBurst),
		now:       now,
		buckets:   make(map[string]*bucket),
	}
}

// middleware enforces the budget. Mounted on /api/v1 and /ota/v1 and
// deliberately NOT on the health probes: a kubelet that gets 429 from a
// liveness check restarts a healthy pod, which converts an overload into an
// outage.
func (l *ipRateLimiter) middleware(next http.Handler) http.Handler {
	if !l.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retryAfter, ok := l.take(clientIP(r))
		if !ok {
			// Retry-After is the contract that makes a 429 polite: the same
			// header this service's own Lokalise client honours from the
			// other side.
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
			render.Status(r, http.StatusTooManyRequests)
			render.JSON(w, r, errorResponse{
				Error:   "rate_limited",
				Details: "too many requests from this client; retry after the indicated delay",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// take spends one token for the client, refilling by elapsed time first.
// When the bucket is empty it reports how many whole seconds until a token
// accrues.
func (l *ipRateLimiter) take(client string) (retryAfterSeconds int, ok bool) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, exists := l.buckets[client]
	if !exists {
		if len(l.buckets) >= bucketPruneAt {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[client] = b
	}

	// Continuous refill, capped at burst.
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSecond)
	b.last = now

	if b.tokens < 1 {
		wait := (1 - b.tokens) / l.perSecond
		return int(math.Ceil(wait)), false
	}
	b.tokens--
	return 0, true
}

// prune drops buckets that have refilled to full — a full bucket carries no
// state a fresh one would not. Called with the lock held.
func (l *ipRateLimiter) prune(now time.Time) {
	for key, b := range l.buckets {
		if math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSecond) >= l.burst {
			delete(l.buckets, key)
		}
	}
}

// clientIP returns the host part of RemoteAddr, which middleware.RealIP has
// already resolved from the proxy headers. The port is stripped because it
// changes per connection and would give every request its own bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
