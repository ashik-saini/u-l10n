package repository

import (
	"context"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// LocaleRepository reads the locale dimension. Locales are seeded by migration
// V1.00 and are not writable at runtime: adding one is a schema change, because
// the export layout depends on it.
type LocaleRepository interface {
	List(ctx context.Context, tx *gorm.DB) ([]model.Locale, error)
	ByCode(ctx context.Context, tx *gorm.DB, code string) (model.Locale, error)
}

type localeRepository struct{ base }

func ProvideLocaleRepository(connector database.GORMConnector) LocaleRepository {
	return &localeRepository{base{connector: connector}}
}

const localeColumns = `id, code, flutter_dir, android_values_dir, ios_lproj, sort_order`

func (r *localeRepository) List(ctx context.Context, tx *gorm.DB) ([]model.Locale, error) {
	rows, err := r.db(ctx, tx).Raw(
		`SELECT ` + localeColumns + ` FROM locales ORDER BY sort_order`).Rows()
	if err != nil {
		return nil, fmt.Errorf("list locales: %w", err)
	}
	defer rows.Close()

	var out []model.Locale
	for rows.Next() {
		var l model.Locale
		if err := rows.Scan(&l.ID, &l.Code, &l.FlutterDir,
			&l.AndroidValuesDir, &l.IOSLproj, &l.SortOrder); err != nil {
			return nil, fmt.Errorf("scan locale: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *localeRepository) ByCode(ctx context.Context, tx *gorm.DB, code string) (model.Locale, error) {
	var l model.Locale
	row := r.db(ctx, tx).Raw(
		`SELECT `+localeColumns+` FROM locales WHERE code = ?`, code).Row()

	switch err := row.Scan(&l.ID, &l.Code, &l.FlutterDir,
		&l.AndroidValuesDir, &l.IOSLproj, &l.SortOrder); {
	case err == nil:
		return l, nil
	case isNoRows(err):
		return l, fmt.Errorf("locale %q: %w", code, ErrNotFound)
	default:
		return l, fmt.Errorf("locale %q: %w", code, err)
	}
}
