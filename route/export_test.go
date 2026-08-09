package route

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/exportsvc"
)

// stubExportRows is an ExportRowReader with a fixed answer.
type stubExportRows struct {
	rows []repository.ExportRow
	err  error
}

func (s *stubExportRows) ForExport(
	context.Context, *gorm.DB, int16, model.Platform,
) ([]repository.ExportRow, error) {
	return s.rows, s.err
}

func exportHandler(rows *stubExportRows) *Handler {
	locales := &stubLocales{byCode: map[string]model.Locale{
		"en_SG": {ID: 1, Code: "en_SG", FlutterDir: "en_SG",
			AndroidValuesDir: "values", IOSLproj: "en-SG.lproj"},
	}}
	return &Handler{exports: exportsvc.ProvideService(locales, rows)}
}

// TestExportCollisionIsBadRequest.
//
// An Android name collision is the caller's data problem — two keys that
// transform to one resource name — and must map to 400 through the ErrBadRequest
// sentinel, not through anybody matching message text.
func TestExportCollisionIsBadRequest(t *testing.T) {
	h := exportHandler(&stubExportRows{rows: []repository.ExportRow{
		{Key: "foo-bar", Value: "a", Found: true},
		{Key: "foo_bar", Value: "b", Found: true},
	}})

	rec := httptest.NewRecorder()
	h.Export(rec, httptest.NewRequest(http.MethodGet, "/api/v1/export?format=xml", nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "collision")
}

// TestExportUnrelatedErrorMentioningCollisionIs500.
//
// The word "collision" in an infrastructure failure must not demote it to a
// client mistake: classification is by sentinel, never by message text.
func TestExportUnrelatedErrorMentioningCollisionIs500(t *testing.T) {
	h := exportHandler(&stubExportRows{
		err: errors.New("read replica: hash collision in connection pool"),
	})

	rec := httptest.NewRecorder()
	h.Export(rec, httptest.NewRequest(http.MethodGet, "/api/v1/export?format=xml", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "export_failed")
}
