package route

import (
	"context"
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
// The client key is derived by clientIP, deliberately NOT from r.RemoteAddr:
// by the time the limiter runs, middleware.RealIP has rewritten RemoteAddr
// from X-Real-IP or the LEFTMOST X-Forwarded-For entry — both of which the
// client chooses. Envoy and Istio APPEND the peer address to X-Forwarded-For;
// they do not strip client-supplied leading entries, so a limiter keyed on
// RealIP's answer hands every request a fresh bucket: the limit never fires
// and the bucket map grows without bound. clientIP instead walks
// X-Forwarded-For right to left past the mesh's own private hops, and falls
// back to the socket address captured before RealIP ran. RealIP itself stays
// mounted — its answer is fine for logs and traces, just not for a budget.
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

// ctxKeySocketAddr carries the RemoteAddr of the underlying connection,
// captured before middleware.RealIP rewrites it from client-chosen headers.
type ctxKeySocketAddr struct{}

// captureSocketAddr stores the socket's RemoteAddr in the request context.
// It is mounted BEFORE middleware.RealIP in ProvideRoutes — after RealIP runs
// the original address is gone, and it is the only per-request fact the
// client cannot forge.
func captureSocketAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), ctxKeySocketAddr{}, r.RemoteAddr)))
	})
}

// rateLimitUnknownClient is the bucket for every request whose client cannot
// be derived at all. One shared key rather than one per garbage value, so
// unparseable input cannot grow the map.
const rateLimitUnknownClient = "unknown-client"

// clientIP derives the limiter key. In order:
//
//  1. Walk X-Forwarded-For RIGHT to LEFT. The rightmost entries are the ones
//     our own infrastructure appended — Envoy appends the peer address and
//     never strips what the client sent — so loopback, private and link-local
//     hops are skipped as trusted infra, and the first public IP is the
//     client as the edge saw it. Everything left of that is client-supplied
//     and is never consulted; an entry that does not parse ends the walk for
//     the same reason.
//  2. Otherwise, the host part of the ORIGINAL socket RemoteAddr, captured by
//     captureSocketAddr before middleware.RealIP rewrote it.
//  3. A key that still does not parse as an IP lands in the shared sentinel
//     bucket, which bounds memory under garbage input.
//
// Assumption: every hop between the client and this service sits on private
// address space. A trusted proxy with a public address would be taken for the
// client and throttled on its aggregate traffic — a safe failure, unlike the
// reverse, which was the bug: trusting a client-chosen entry means no limit
// at all.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		entries := splitAndTrim(xff, ',')
		for i := len(entries) - 1; i >= 0; i-- {
			ip := net.ParseIP(entries[i])
			if ip == nil {
				break
			}
			if ip.IsLoopback() || ip.IsPrivate() ||
				ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			return ip.String()
		}
	}

	raw, _ := r.Context().Value(ctxKeySocketAddr{}).(string)
	if raw == "" {
		// Only reachable when the limiter runs outside ProvideRoutes' chain —
		// unit tests — where RemoteAddr has not been rewritten.
		raw = r.RemoteAddr
	}
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		host = raw
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return rateLimitUnknownClient
}
