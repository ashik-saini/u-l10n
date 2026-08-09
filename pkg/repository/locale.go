package repository

import (
	"context"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// LocaleRepository reads and writes the locale dimension.
//
// Locales are per project and admin-managed: adding one to a project must be
// an action in the portal, not a migration and a deploy. Adding a locale
// writes no translation rows — absent means untranslated, so a new locale
// starts empty and fills in as translators work.
type LocaleRepository interface {
	List(ctx context.Context, tx *gorm.DB, projectID int16, includeArchived bool) ([]model.Locale, error)
	ByCode(ctx context.Context, tx *gorm.DB, projectID int16, code string) (model.Locale, error)
}

type localeRepository struct{ base }

func ProvideLocaleRepository(connector database.GORMConnector) LocaleRepository {
	return &localeRepository{base{connector: connector}}
}

const localeColumns = `id, project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order, status`

func (r *localeRepository) List(
	ctx context.Context, tx *gorm.DB, projectID int16, includeArchived bool,
) ([]model.Locale, error) {
	q := `SELECT ` + localeColumns + ` FROM locales WHERE project_id = ?`
	if !includeArchived {
		q += ` AND status = 'active'`
	}
	q += ` ORDER BY sort_order`

	rows, err := r.db(ctx, tx).Raw(q, projectID).Rows()
	if err != nil {
		return nil, fmt.Errorf("list locales for project %d: %w", projectID, err)
	}
	defer rows.Close()

	var out []model.Locale
	for rows.Next() {
		var l model.Locale
		if err := rows.Scan(&l.ID, &l.ProjectID, &l.Code, &l.FlutterDir,
			&l.AndroidValuesDir, &l.IOSLproj, &l.SortOrder, &l.Status); err != nil {
			return nil, fmt.Errorf("scan locale: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *localeRepository) ByCode(
	ctx context.Context, tx *gorm.DB, projectID int16, code string,
) (model.Locale, error) {
	var l model.Locale
	row := r.db(ctx, tx).Raw(
		`SELECT `+localeColumns+` FROM locales WHERE project_id = ? AND code = ?`,
		projectID, code).Row()

	switch err := row.Scan(&l.ID, &l.ProjectID, &l.Code, &l.FlutterDir,
		&l.AndroidValuesDir, &l.IOSLproj, &l.SortOrder, &l.Status); {
	case err == nil:
		return l, nil
	case isNoRows(err):
		return l, fmt.Errorf("locale %q: %w", code, ErrNotFound)
	default:
		return l, fmt.Errorf("locale %q: %w", code, err)
	}
}
