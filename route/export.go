package route

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/export"
	"github.com/yougroupteam/u-l10n/pkg/service/exportsvc"
)

// Export renders the requested format for the requested locales as a zip.
//
//	GET /api/v1/export?format=json&locales=en-SG,en-MY&empty_mode=include
//
// Synchronous: the response body IS the archive. At ~36,000 values this
// completes faster than the HTTP round trip, so there is no job to poll and the
// u-mobile scripts collapse to a single authenticated curl piped into unzip.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	req, err := parseExportRequest(r)
	if err != nil {
		h.badRequest(w, r, err)
		return
	}

	archive, err := h.exports.Zip(r.Context(), req)
	if err != nil {
		// A collision or an unknown locale is the caller's problem; anything
		// else is ours.
		if errors.Is(err, exportsvc.ErrBadRequest) || strings.Contains(err.Error(), "collision") {
			h.badRequest(w, r, err)
			return
		}
		log.Errore(r.Context(), "export failed", err)
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, errorResponse{Error: "export_failed"})
		return
	}

	filename := fmt.Sprintf("u-l10n-%s-%s.zip", req.Format, time.Now().UTC().Format("20060102T150405Z"))

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// Never cache: the archive reflects mutable state, and a stale export
	// silently ships old copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(archive); err != nil {
		// The status line is already sent, so this can only be logged.
		log.Errore(r.Context(), "failed writing export body", err)
	}
}

type errorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

func (h *Handler) badRequest(w http.ResponseWriter, r *http.Request, err error) {
	render.Status(r, http.StatusBadRequest)
	render.JSON(w, r, errorResponse{Error: "bad_request", Details: err.Error()})
}

// parseExportRequest validates query parameters, rejecting anything unknown
// rather than ignoring it. A script that misspells a parameter must be told,
// not silently handed a default export.
func parseExportRequest(r *http.Request) (exportsvc.Request, error) {
	q := r.URL.Query()

	allowed := map[string]bool{
		"format": true, "locales": true, "empty_mode": true, "line_ending": true,
	}
	for name := range q {
		if !allowed[name] {
			return exportsvc.Request{}, fmt.Errorf(
				"%w: unknown parameter %q", exportsvc.ErrBadRequest, name)
		}
	}

	req := exportsvc.Request{
		Format:    exportsvc.Format(q.Get("format")),
		EmptyMode: exportsvc.EmptyMode(q.Get("empty_mode")),
	}
	if req.Format == "" {
		return req, fmt.Errorf("%w: format is required (json, xml or strings)", exportsvc.ErrBadRequest)
	}

	if raw := q.Get("locales"); raw != "" {
		for _, code := range strings.Split(raw, ",") {
			if code = strings.TrimSpace(code); code != "" {
				req.Locales = append(req.Locales, code)
			}
		}
	}

	switch req.EmptyMode {
	case "", exportsvc.EmptyModeInclude, exportsvc.EmptyModeSkip:
	default:
		return req, fmt.Errorf("%w: empty_mode must be include or skip_empty, got %q",
			exportsvc.ErrBadRequest, req.EmptyMode)
	}

	switch q.Get("line_ending") {
	case "", "lf":
		req.LineEnding = export.LF
	case "crlf":
		req.LineEnding = export.CRLF
	default:
		return req, fmt.Errorf("%w: line_ending must be lf or crlf", exportsvc.ErrBadRequest)
	}

	return req, nil
}
