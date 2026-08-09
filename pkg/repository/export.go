package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// ExportRow is one key/value pair in export order.
type ExportRow struct {
	Key        string
	Value      string
	RenderHint model.RenderHint
	// Found reports whether a translations row existed. It distinguishes
	// untranslated (no row) from deliberately empty (a row whose value is ""),
	// which the export's empty_mode depends on and which a plain string cannot
	// express.
	Found bool
}

// ExportRowReader reads a whole locale in one query.
type ExportRowReader interface {
	ForExport(ctx context.Context, tx *gorm.DB, localeID int16, platform model.Platform) ([]ExportRow, error)
}

type exportRowReader struct{ base }

func ProvideExportRowReader(connector database.GORMConnector) ExportRowReader {
	return &exportRowReader{base{connector: connector}}
}

// forExportSQL reads every active key for a platform with its value for one
// locale, in export order.
//
// A LEFT JOIN, not an inner join: a key with no translation for this locale must
// still appear, because whether to emit it is the export's decision (empty_mode)
// and not something to silently drop here. `t.key_id IS NOT NULL` is what tells
// the caller a row existed, since a NULL value and an empty-string value are
// otherwise indistinguishable once scanned.
//
// ORDER BY sort_index because export order is DATA — it must reproduce
// Lokalise's non-alphabetical ordering or every generated file is a
// multi-thousand-line diff against the committed one.
const forExportSQL = `
SELECT k.name,
       t.value,
       COALESCE(t.render_hint, 'plain') AS render_hint,
       (t.key_id IS NOT NULL)          AS found
  FROM keys k
  LEFT JOIN translations t
    ON t.key_id = k.id AND t.locale_id = $1
 WHERE k.status = 'active'
   AND $2 = ANY(k.platforms)
 ORDER BY k.sort_index`

func (r *exportRowReader) ForExport(
	ctx context.Context, tx *gorm.DB, localeID int16, platform model.Platform,
) ([]ExportRow, error) {
	rows, err := r.db(ctx, tx).Raw(forExportSQL, localeID, string(platform)).Rows()
	if err != nil {
		return nil, fmt.Errorf("read export rows: %w", err)
	}
	defer rows.Close()

	out := make([]ExportRow, 0, 6500)
	for rows.Next() {
		var (
			name  string
			value sql.NullString
			hint  string
			found bool
		)
		if err := rows.Scan(&name, &value, &hint, &found); err != nil {
			return nil, fmt.Errorf("scan export row: %w", err)
		}
		out = append(out, ExportRow{
			Key: name,
			// value.String is "" when NULL, which is correct for both cases:
			// an untranslated key exports as "" under empty_mode=include, and
			// a deliberately empty one exports as "" too. Found is what keeps
			// them distinguishable.
			Value:      value.String,
			RenderHint: model.RenderHint(hint),
			Found:      found,
		})
	}
	return out, rows.Err()
}

var _ = pq.Array
