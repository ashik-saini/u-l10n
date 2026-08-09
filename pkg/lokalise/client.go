// Package lokalise is a read-only client for the Lokalise API v2.
//
// It exists to be run ONCE per environment at cutover, so it optimises for
// being a well-behaved guest rather than for throughput: it self-limits, it
// honours Retry-After, and it is resumable. A full run over ~6,300 keys takes
// about a minute.
//
// The client deliberately fetches CONTENT only. It cannot tell an empty
// translation from an absent one — the API returns "" for both — so presence is
// resolved separately from the file exports, which are the only source that
// knows. See pkg/service/importsvc.
package lokalise

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultBaseURL = "https://api.lokalise.com/api2"

	// defaultRate is deliberately below Lokalise's published ceiling. Being
	// throttled mid-import is worse than taking an extra thirty seconds.
	defaultRate = 4

	// maxAttempts bounds retries so a permanently failing request surfaces
	// rather than looping until someone notices.
	maxAttempts = 5
)

// Client talks to the Lokalise API.
type Client struct {
	baseURL   string
	token     string
	projectID string
	http      *http.Client

	// limiter is a token bucket: one slot per interval, refilled by a ticker.
	// A blocking channel is enough here and avoids a dependency for what is a
	// dozen lines.
	limiter <-chan time.Time
	stop    func()
}

// Option configures a Client.
type Option func(*Client)

func WithBaseURL(u string) Option          { return func(c *Client) { c.baseURL = u } }
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithRatePerSecond overrides the self-imposed request rate.
func WithRatePerSecond(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.setRate(n)
		}
	}
}

// New builds a client. Call Close when finished to stop the rate limiter.
func New(token, projectID string, opts ...Option) *Client {
	c := &Client{
		baseURL:   defaultBaseURL,
		token:     token,
		projectID: projectID,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
	c.setRate(defaultRate)
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) setRate(perSecond int) {
	if c.stop != nil {
		c.stop()
	}
	ticker := time.NewTicker(time.Second / time.Duration(perSecond))
	c.limiter = ticker.C
	c.stop = ticker.Stop
}

// Close releases the rate limiter's ticker.
func (c *Client) Close() {
	if c.stop != nil {
		c.stop()
	}
}

// Language is one locale configured on the project.
type Language struct {
	LangISO string `json:"lang_iso"`
	Name    string `json:"lang_name"`
}

// Key is one key with its translations.
type Key struct {
	KeyID        int64         `json:"key_id"`
	KeyName      KeyName       `json:"key_name"`
	Description  string        `json:"description"`
	Platforms    []string      `json:"platforms"`
	Translations []Translation `json:"translations"`
}

// KeyName is per-platform. Lokalise returns an object here, and the platform
// variants differ — Android names are transformed, iOS names are not — so this
// cannot be collapsed to a single string without losing the overrides.
type KeyName struct {
	Web     string `json:"web"`
	Android string `json:"android"`
	IOS     string `json:"ios"`
	Other   string `json:"other"`
}

// Canonical returns the name to store.
//
// 'web' maps to the flutter platform and carries the untransformed name, so it
// is preferred; the others are fallbacks for keys that exist on one platform
// only.
func (n KeyName) Canonical() string {
	for _, candidate := range []string{n.Web, n.IOS, n.Other, n.Android} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// Translation is one value for one language.
//
// Value is "" both for a deliberately-blank string and for one nobody has
// translated. That ambiguity is unresolvable here and is why the importer needs
// the file exports as a presence oracle.
type Translation struct {
	LangISO string `json:"language_iso"`
	Value   string `json:"translation"`
}

// Languages returns the project's configured locales.
func (c *Client) Languages(ctx context.Context) ([]Language, error) {
	var out struct {
		Languages []Language `json:"languages"`
	}
	if err := c.get(ctx, fmt.Sprintf("/projects/%s/languages?limit=100", c.projectID), &out); err != nil {
		return nil, err
	}
	return out.Languages, nil
}

// Keys returns every key with its translations, following pagination.
func (c *Client) Keys(ctx context.Context) ([]Key, error) {
	const pageSize = 500

	var all []Key
	for page := 1; ; page++ {
		var out struct {
			Keys []Key `json:"keys"`
		}
		path := fmt.Sprintf("/projects/%s/keys?include_translations=1&limit=%d&page=%d",
			c.projectID, pageSize, page)
		if err := c.get(ctx, path, &out); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}

		all = append(all, out.Keys...)

		// A short page is the last page. Trusting a total-count header would
		// mean trusting it to be consistent with the rows actually returned.
		if len(out.Keys) < pageSize {
			return all, nil
		}
	}
}

// get performs a rate-limited, retrying GET.
func (c *Client) get(ctx context.Context, path string, into interface{}) error {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Wait for a token before every attempt, retries included — a retry
		// storm is exactly when politeness matters most.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.limiter:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("X-Api-Token", c.token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			// Closed explicitly, not deferred: this is inside the retry loop,
			// and a deferred Close would not fire until the whole function
			// returns — stacking one per attempt. The other branches already
			// close before they continue, so the body is closed on every path.
			err := json.NewDecoder(resp.Body).Decode(into)
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("decode %s: %w", path, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			wait := retryAfter(resp)
			resp.Body.Close()
			lastErr = fmt.Errorf("rate limited on %s", path)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}

		case resp.StatusCode >= 500:
			// Server-side and probably transient.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: %s: %s", path, resp.Status, body)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt)):
			}

		default:
			// 4xx other than 429 will not improve with retrying.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return fmt.Errorf("%s: %s: %s", path, resp.Status, body)
		}
	}

	return fmt.Errorf("gave up after %d attempts: %w", maxAttempts, lastErr)
}

// retryAfter honours the server's own guidance, falling back to a sane default
// when the header is missing or unparseable.
func retryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Second
}

// backoff grows linearly rather than exponentially: this is a one-shot import
// against a known-good API, so a fast third attempt beats a patient one.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * 500 * time.Millisecond
}
