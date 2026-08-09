package exportsvc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// stubLocales is a LocaleRepository with a fixed list.
type stubLocales struct{ locales []model.Locale }

func (s *stubLocales) List(context.Context, *gorm.DB, int16, bool) ([]model.Locale, error) {
	return s.locales, nil
}

func (s *stubLocales) ByCode(_ context.Context, _ *gorm.DB, _ int16, code string) (model.Locale, error) {
	for _, l := range s.locales {
		if l.Code == code {
			return l, nil
		}
	}
	return model.Locale{}, fmt.Errorf("locale %q: %w", code, repository.ErrNotFound)
}

// stubRows is an ExportRowReader with a fixed answer.
type stubRows struct {
	rows []repository.ExportRow
	err  error
}

func (s *stubRows) ForExport(
	context.Context, *gorm.DB, int16, model.Platform,
) ([]repository.ExportRow, error) {
	return s.rows, s.err
}

func testService(rows *stubRows) *Service {
	return ProvideService(&stubLocales{locales: []model.Locale{
		{ID: 1, Code: "en_SG", FlutterDir: "en_SG",
			AndroidValuesDir: "values", IOSLproj: "en-SG.lproj", SortOrder: 1},
	}}, rows)
}

// TestZipDeduplicatesRequestedLocales.
//
// ?locales=en_SG&locales=en_SG must produce ONE archive entry. zip.Writer
// happily writes two files under the same path, and which one an unzip leaves
// behind is the tool's choice — a silent coin flip inside a release artifact.
func TestZipDeduplicatesRequestedLocales(t *testing.T) {
	svc := testService(&stubRows{rows: []repository.ExportRow{
		{Key: "a", Value: "one", Found: true},
	}})

	archive, err := svc.Zip(context.Background(), Request{
		Format:  FormatJSON,
		Locales: []string{"en_SG", "en_SG"},
	})
	require.NoError(t, err)

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.Len(t, zr.File, 1, "a repeated code must not duplicate the entry")
	assert.Equal(t, "en_SG/en_SG.json", zr.File[0].Name)
}

// TestZipCollisionCarriesTheBadRequestSentinel.
//
// The handler maps caller mistakes to 400 via errors.Is on ErrBadRequest, so
// the collision refusal must carry the sentinel — matching message text in the
// handler is the bug this pins against.
func TestZipCollisionCarriesTheBadRequestSentinel(t *testing.T) {
	svc := testService(&stubRows{rows: []repository.ExportRow{
		{Key: "foo-bar", Value: "a", Found: true},
		{Key: "foo_bar", Value: "b", Found: true},
	}})

	_, err := svc.Zip(context.Background(), Request{Format: FormatXML})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadRequest)
	assert.Contains(t, err.Error(), "collision")
	assert.Contains(t, err.Error(), "en_SG", "the failing locale must be named")
}

// TestZipInfrastructureErrorIsNotABadRequest: only the caller's mistakes carry
// the sentinel; a failing read stays a 500-shaped error.
func TestZipInfrastructureErrorIsNotABadRequest(t *testing.T) {
	svc := testService(&stubRows{err: errors.New("connection refused")})

	_, err := svc.Zip(context.Background(), Request{Format: FormatJSON})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBadRequest)
}
