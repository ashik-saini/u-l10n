// Package exportsvc assembles export archives from the store.
//
// The response IS the file: 36,000 values serialise in well under a second, so
// there is no job, no polling and no status endpoint. That deletes the entire
// async dance the current Lokalise scripts carry — a 44-line shell function
// becomes about ten.
package exportsvc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/yougroupteam/u-l10n/pkg/export"
	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// Format selects which platform's files to emit. One format per request, which
// matches how the u-mobile scripts already call Lokalise.
type Format string

const (
	FormatJSON    Format = "json"    // Flutter
	FormatXML     Format = "xml"     // Android
	FormatStrings Format = "strings" // iOS
)

// platform returns the platform implied by the format. Format implies the
// platform filter — asking for XML can only mean the Android key set — so a
// caller cannot accidentally request a mismatched combination.
func (f Format) platform() (model.Platform, error) {
	switch f {
	case FormatJSON:
		return model.PlatformFlutter, nil
	case FormatXML:
		return model.PlatformAndroid, nil
	case FormatStrings:
		return model.PlatformIOS, nil
	default:
		// Wrapped so the handler maps it to 400. An unrecognised format is the
		// caller's mistake, and returning 500 would send an on-call engineer
		// looking for a fault that does not exist.
		return "", fmt.Errorf("%w: unknown format %q, want json, xml or strings", ErrBadRequest, f)
	}
}

// EmptyMode decides what happens to a key with no translation for a locale.
type EmptyMode string

const (
	// EmptyModeInclude emits the key with an empty value. This is the default
	// because it reproduces the files committed today.
	EmptyModeInclude EmptyMode = "include"
	// EmptyModeSkip omits the key entirely.
	EmptyModeSkip EmptyMode = "skip_empty"
)

// Request describes one export.
type Request struct {
	Format     Format
	Locales    []string // empty means every locale
	EmptyMode  EmptyMode
	LineEnding export.LineEnding // .strings only
}

// Service builds export archives.
type Service struct {
	locales repository.LocaleRepository
	rows    repository.ExportRowReader
}

func ProvideService(
	locales repository.LocaleRepository,
	rows repository.ExportRowReader,
) *Service {
	return &Service{locales: locales, rows: rows}
}

// Zip renders the requested format for every selected locale into a zip.
//
// The archive layout reproduces Lokalise's exact internal structure —
// en_SG/en_SG.json, values-en-rMY/strings.xml, en-MY.lproj/Localizable.strings —
// because run.sh has 22 `cp -rf` mappings keyed on those paths. The layout comes
// from the locales table, so the serializers never hardcode a directory.
func (s *Service) Zip(ctx context.Context, req Request) ([]byte, error) {
	platform, err := req.Format.platform()
	if err != nil {
		return nil, err
	}
	if req.EmptyMode == "" {
		req.EmptyMode = EmptyModeInclude
	}

	locales, err := s.selectLocales(ctx, req.Locales)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, locale := range locales {
		entries, err := s.entriesFor(ctx, locale, platform, req.EmptyMode)
		if err != nil {
			return nil, err
		}

		// Fail the WHOLE export on an Android name collision, before a single
		// byte is written. Two distinct keys can transform to the same resource
		// name, and the second silently overwrites the first in the generated
		// file. A warning here would be a warning nobody reads; the failure mode
		// is one string quietly replacing another in a shipped app.
		if platform == model.PlatformAndroid {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Key
			}
			// Wrapped in ErrBadRequest: a collision is the caller's data — two
			// keys they own transforming to one resource name — and the handler
			// classifies by sentinel, never by message text.
			if err := export.CheckAndroidNames(names); err != nil {
				return nil, fmt.Errorf("%w: locale %s: %v", ErrBadRequest, locale.Code, err)
			}
		}

		name, body := s.render(req, locale, entries)

		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("create %s in archive: %w", name, err)
		}
		if _, err := w.Write(body); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finalise archive: %w", err)
	}
	return buf.Bytes(), nil
}

// render produces the archive path and file body for one locale.
func (s *Service) render(req Request, locale model.Locale, entries []export.Entry) (string, []byte) {
	switch req.Format {
	case FormatJSON:
		// flutter/en_SG/en_SG.json — the directory and the basename match.
		return path.Join(locale.FlutterDir, locale.FlutterDir+".json"), export.JSON(entries)

	case FormatXML:
		// Android resource names are derived at write time; the canonical key
		// stays in the store.
		named := make([]export.Entry, len(entries))
		for i, e := range entries {
			named[i] = export.Entry{
				Key:   export.AndroidName(e.Key),
				Value: e.Value,
				CDATA: e.CDATA,
			}
		}
		return path.Join(locale.AndroidValuesDir, "strings.xml"), export.XML(named)

	default: // FormatStrings — iOS keys are not transformed.
		return path.Join(locale.IOSLproj, "Localizable.strings"),
			export.Strings(entries, export.StringsOptions{LineEnding: req.LineEnding})
	}
}

// entriesFor reads one locale's rows in export order.
func (s *Service) entriesFor(
	ctx context.Context, locale model.Locale, platform model.Platform, mode EmptyMode,
) ([]export.Entry, error) {
	rows, err := s.rows.ForExport(ctx, nil, locale.ID, platform)
	if err != nil {
		return nil, fmt.Errorf("read rows for %s: %w", locale.Code, err)
	}

	entries := make([]export.Entry, 0, len(rows))
	for _, r := range rows {
		// r.Found distinguishes untranslated (no row) from deliberately empty
		// (a row whose value is ""). Only the former is affected by EmptyMode;
		// an explicit "" is content and is always emitted.
		if !r.Found && mode == EmptyModeSkip {
			continue
		}
		entries = append(entries, export.Entry{
			Key:   r.Key,
			Value: r.Value,
			CDATA: r.RenderHint == model.RenderHintCDATA,
		})
	}
	return entries, nil
}

// selectLocales resolves the requested codes, or returns all of them.
func (s *Service) selectLocales(ctx context.Context, codes []string) ([]model.Locale, error) {
	// TODO(plan-2): the scope arrives from the request path once routes are
	// project-prefixed. Hardcoded to YouTrip until then.
	all, err := s.locales.List(ctx, nil, 1, false)
	if err != nil {
		return nil, err
	}
	if len(codes) == 0 {
		return all, nil
	}

	byCode := make(map[string]model.Locale, len(all))
	for _, l := range all {
		byCode[l.Code] = l
	}

	out := make([]model.Locale, 0, len(codes))
	var unknown []string
	// seen deduplicates repeated codes — ?locales=en_SG&locales=en_SG must not
	// write two archive entries under one path, leaving unzip to pick a winner.
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		l, ok := byCode[code]
		if !ok {
			unknown = append(unknown, code)
			continue
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, l)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		// Reject rather than silently exporting a subset: a script that
		// typos a locale must not quietly ship five files instead of six.
		return nil, fmt.Errorf("%w: unknown locales %v", ErrBadRequest, unknown)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

// ErrBadRequest marks a caller error so the handler can map it to 400 rather
// than 500.
var ErrBadRequest = errors.New("bad request")
