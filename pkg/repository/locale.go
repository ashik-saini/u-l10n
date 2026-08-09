package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// Locale statuses. A locale is archived, never deleted: translations and
// history reference it, and V1.08 already settled that an audit trail
// outlives its subject.
const (
	LocaleActive   = "active"
	LocaleArchived = "archived"
)

// ErrLocaleCodeTaken is returned when a project already has that locale.
// Exported so the handler answers 409 rather than letting a unique violation
// surface as a 500.
var ErrLocaleCodeTaken = errors.New("locale code already in use for this project")

// ErrLocaleDirectoryTaken is returned when a project already has another
// locale claiming the same flutter/android/ios export directory. UNIQUE
// (project_id, flutter_dir|android_values_dir|ios_lproj) — added in V1.10 —
// is the authority here; Create and Update do not pre-check it, because the
// other locale can appear between a check and the write.
var ErrLocaleDirectoryTaken = errors.New("locale export directory already in use for this project")

// LocaleRepository reads and writes the locale dimension.
//
// Locales are per project and admin-managed: adding one to a project must be
// an action in the portal, not a migration and a deploy. Adding a locale
// writes no translation rows — absent means untranslated, so a new locale
// starts empty and fills in as translators work.
//
// There is no Delete. Translations and history reference locales, so the
// only lifecycle move is archiving via Update's status field.
type LocaleRepository interface {
	List(ctx context.Context, tx *gorm.DB, projectID int16, includeArchived bool) ([]model.Locale, error)
	ByCode(ctx context.Context, tx *gorm.DB, projectID int16, code string) (model.Locale, error)
	Create(ctx context.Context, tx *gorm.DB, l model.Locale) (model.Locale, error)
	// Update takes the OLD code as a lookup key, not a field to write: the
	// code is deliberately not updatable through this method, because it is
	// the identifier callers address the locale by and renaming it would
	// silently orphan every translation and history row that references it.
	Update(ctx context.Context, tx *gorm.DB, projectID int16, code string, l model.Locale) (model.Locale, error)
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

// Create adds a locale to a project. It writes no translation rows — absent
// means untranslated, so a new locale starts empty and fills in as
// translators work.
//
// ON CONFLICT targets ONLY (project_id, code): the other three uniqueness
// constraints on this table (flutter_dir, android_values_dir, ios_lproj) are
// deliberately not part of the conflict target, so a collision on one of
// those surfaces as a real driver error (23505) rather than a silently
// skipped INSERT — isUniqueViolation below is what turns that into
// ErrLocaleDirectoryTaken instead of a 500.
func (r *localeRepository) Create(ctx context.Context, tx *gorm.DB, l model.Locale) (model.Locale, error) {
	status := l.Status
	if status == "" {
		status = LocaleActive
	}
	row := r.db(ctx, tx).Raw(
		`INSERT INTO locales
		     (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, code) DO NOTHING
		 RETURNING `+localeColumns,
		l.ProjectID, l.Code, l.FlutterDir, l.AndroidValuesDir,
		l.IOSLproj, l.SortOrder, status).Row()

	var out model.Locale
	switch err := row.Scan(&out.ID, &out.ProjectID, &out.Code, &out.FlutterDir,
		&out.AndroidValuesDir, &out.IOSLproj, &out.SortOrder, &out.Status); {
	case err == nil:
		return out, nil
	case isUniqueViolation(err):
		return model.Locale{}, fmt.Errorf("create locale %q for project %d: %w",
			l.Code, l.ProjectID, ErrLocaleDirectoryTaken)
	case isNoRows(err):
		// DO NOTHING returned no row: the (project_id, code) pair is taken.
		return model.Locale{}, fmt.Errorf("create locale %q for project %d: %w",
			l.Code, l.ProjectID, ErrLocaleCodeTaken)
	default:
		return model.Locale{}, fmt.Errorf("create locale %q for project %d: %w",
			l.Code, l.ProjectID, err)
	}
}

// Update changes a locale's export directories, sort order and status.
//
// It is addressed by the OLD code, which is never itself a SET target — see
// the interface doc. Status carries the only lifecycle move this table has:
// there is no Delete, because translations and history reference the row.
func (r *localeRepository) Update(
	ctx context.Context, tx *gorm.DB, projectID int16, code string, l model.Locale,
) (model.Locale, error) {
	row := r.db(ctx, tx).Raw(
		`UPDATE locales
		    SET flutter_dir = ?, android_values_dir = ?, ios_lproj = ?,
		        sort_order = ?, status = ?
		  WHERE project_id = ? AND code = ?
		 RETURNING `+localeColumns,
		l.FlutterDir, l.AndroidValuesDir, l.IOSLproj,
		l.SortOrder, l.Status, projectID, code).Row()

	var out model.Locale
	switch err := row.Scan(&out.ID, &out.ProjectID, &out.Code, &out.FlutterDir,
		&out.AndroidValuesDir, &out.IOSLproj, &out.SortOrder, &out.Status); {
	case err == nil:
		return out, nil
	case isUniqueViolation(err):
		return model.Locale{}, fmt.Errorf("update locale %q for project %d: %w",
			code, projectID, ErrLocaleDirectoryTaken)
	case isNoRows(err):
		return model.Locale{}, fmt.Errorf("locale %q: %w", code, ErrNotFound)
	default:
		return model.Locale{}, fmt.Errorf("update locale %q for project %d: %w",
			code, projectID, err)
	}
}
