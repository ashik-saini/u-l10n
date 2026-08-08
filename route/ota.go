package route

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// otaMaxAge is how long a CDN or client may reuse a bundle without asking.
//
// Five minutes: long enough that the origin sees almost no traffic, short
// enough that a kill switch takes effect quickly. The ETag makes the revalidation
// itself nearly free, so a short max-age costs little.
const otaMaxAge = 300

// OTABundle serves the current string bundle for a locale.
//
//	GET /ota/v1/bundles/en-SG
//	    If-None-Match: "<sha256>"      X-App-Version: 4.12.0
//
// UNAUTHENTICATED, deliberately. The app calls this at launch, before login,
// and the payload is strings that already ship inside the APK/IPA — anyone can
// unzip them. Requiring auth would add a login dependency to the launch path
// for no security gain. The real controls are rate limiting, CDN caching, and
// the by-construction rule that no PII may enter a bundle.
func (h *Handler) OTABundle(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "locale")

	locale, err := h.locales.ByCode(r.Context(), nil, code)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, errorResponse{Error: "unknown_locale"})
			return
		}
		log.Errore(r.Context(), "ota: locale lookup failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "internal_error"})
		return
	}

	bundle, err := h.releases.ServableBundle(r.Context(), nil, locale.ID, r.Header.Get("X-App-Version"))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			if bundle.KillSwitched {
				// 410 Gone is the kill switch. It tells the client to DELETE
				// its cache and fall back to the strings bundled in the binary
				// — which a 404 would not, and a 200-with-empty-body would
				// actively break.
				render.Status(r, http.StatusGone)
				render.JSON(w, r, errorResponse{
					Error:   "release_rolled_back",
					Details: "clear the cached bundle and use the strings shipped in the app",
				})
				return
			}
			// No release yet. Not an error: the app already has its bundled
			// assets and needs nothing from us.
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, errorResponse{Error: "no_release"})
			return
		}
		log.Errore(r.Context(), "ota: bundle lookup failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "internal_error"})
		return
	}

	etag := `"` + bundle.SHA256 + `"`

	// Cache headers go on BOTH the 200 and the 304. A 304 that omits them
	// leaves a CDN with no instruction about how long the revalidated entry
	// stays fresh.
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", otaMaxAge))
	// The response varies by client version, so a shared cache must key on it.
	// Without this a CDN could serve a 4.12-only bundle to a 4.09 client.
	w.Header().Set("Vary", "X-App-Version")

	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		// No body. This is the steady state — a few hundred bytes instead of
		// 60-80KB gzipped, on every app launch.
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(otaResponse{
		Version: bundle.ReleaseVersion,
		Locale:  locale.Code,
		SHA256:  bundle.SHA256,
		Strings: json.RawMessage(bundle.Strings),
	}); err != nil {
		// The status line is already sent; this can only be logged.
		log.Errore(r.Context(), "ota: failed writing bundle", err)
	}
}

type otaResponse struct {
	Version int64           `json:"version"`
	Locale  string          `json:"locale"`
	SHA256  string          `json:"sha256"`
	Strings json.RawMessage `json:"strings"`
}

// matchesETag implements If-None-Match for our single-value case.
//
// A client may legitimately send several, or "*", and a weak validator arrives
// prefixed with W/. Comparing the raw header against our tag would miss all
// three and defeat the 304 path entirely.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}

	for _, candidate := range splitAndTrim(header, ',') {
		if candidate == etag || trimWeakPrefix(candidate) == trimWeakPrefix(etag) {
			return true
		}
	}
	return false
}

func trimWeakPrefix(s string) string {
	if len(s) > 2 && s[0] == 'W' && s[1] == '/' {
		return s[2:]
	}
	return s
}

func splitAndTrim(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == sep {
			part := s[start:i]
			for len(part) > 0 && (part[0] == ' ' || part[0] == '\t') {
				part = part[1:]
			}
			for len(part) > 0 && (part[len(part)-1] == ' ' || part[len(part)-1] == '\t') {
				part = part[:len(part)-1]
			}
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
