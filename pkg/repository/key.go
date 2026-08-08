package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"
	"github.com/lib/pq"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// KeyRepository owns the keys table.
type KeyRepository interface {
	// UpsertByName creates or updates a key identified by its canonical name,
	// returning the row id. Idempotent, so an interrupted import can simply be
	// re-run.
	UpsertByName(ctx context.Context, tx *gorm.DB, k model.Key) (int64, error)

	// IDsByName resolves many names in one query, which keeps the importer from
	// issuing 6,000 round trips.
	IDsByName(ctx context.Context, tx *gorm.DB, names []string) (map[string]int64, error)

	// MaxSortIndex reports the highest sort_index in use, so new keys can be
	// appended after existing ones rather than renumbering.
	MaxSortIndex(ctx context.Context, tx *gorm.DB) (int64, error)

	CountActive(ctx context.Context, tx *gorm.DB) (int, error)
}

type keyRepository struct{ base }

func ProvideKeyRepository(connector database.GORMConnector) KeyRepository {
	return &keyRepository{base{connector: connector}}
}

// upsertKeySQL is hand-written because GORM v1 has no clause.OnConflict — that
// is a v2 API, and clause.OnConflict appears nowhere in this codebase.
//
// The conflict target is the PARTIAL unique index on active names, so the
// statement needs the same WHERE predicate the index carries.
//
// platforms is accumulated rather than replaced: a key seen in the Flutter file
// and later in the Android file belongs to both, and whichever is imported
// second must not erase the first.
const upsertKeySQL = `
INSERT INTO keys (name, description, platforms, android_name, ios_name,
                  status, sort_index, lokalise_key_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
ON CONFLICT (name) WHERE status = 'active' DO UPDATE SET
    description  = COALESCE(NULLIF(EXCLUDED.description, ''), keys.description),
    platforms    = ARRAY(SELECT DISTINCT unnest(keys.platforms || EXCLUDED.platforms) ORDER BY 1),
    android_name = COALESCE(EXCLUDED.android_name, keys.android_name),
    ios_name     = COALESCE(EXCLUDED.ios_name, keys.ios_name),
    version      = keys.version + 1,
    updated_at   = now()
-- Same reasoning as translations: do not churn the version, and therefore the
-- conflict anchor, when a re-run changes nothing. platforms is compared after
-- the accumulate so adding a platform still counts as a change.
WHERE keys.description IS DISTINCT FROM COALESCE(NULLIF(EXCLUDED.description, ''), keys.description)
   OR keys.platforms   IS DISTINCT FROM ARRAY(SELECT DISTINCT unnest(keys.platforms || EXCLUDED.platforms) ORDER BY 1)
   OR keys.android_name IS DISTINCT FROM COALESCE(EXCLUDED.android_name, keys.android_name)
   OR keys.ios_name     IS DISTINCT FROM COALESCE(EXCLUDED.ios_name, keys.ios_name)
RETURNING id`

func (r *keyRepository) UpsertByName(ctx context.Context, tx *gorm.DB, k model.Key) (int64, error) {
	platforms := make([]string, len(k.Platforms))
	for i, p := range k.Platforms {
		platforms[i] = string(p)
	}
	if k.Status == "" {
		k.Status = model.KeyStatusActive
	}

	var id int64
	row := r.db(ctx, tx).Raw(upsertKeySQL,
		k.Name, k.Description, pq.Array(platforms),
		k.AndroidName, k.IOSName, string(k.Status), k.SortIndex, k.LokaliseKeyID).Row()

	switch err := row.Scan(&id); {
	case err == nil:
		return id, nil

	case isNoRows(err):
		// ON CONFLICT DO UPDATE ... WHERE <no change> updates no row, so
		// RETURNING yields nothing. That is the desired outcome — the row is
		// already correct and its version was deliberately not churned — but
		// the caller still needs the id, so read it back.
		row = r.db(ctx, tx).Raw(
			`SELECT id FROM keys WHERE name = ? AND status = 'active'`, k.Name).Row()
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("read back unchanged key %q: %w", k.Name, err)
		}
		return id, nil

	default:
		return 0, fmt.Errorf("upsert key %q: %w", k.Name, err)
	}
}

func (r *keyRepository) IDsByName(ctx context.Context, tx *gorm.DB, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}

	rows, err := r.db(ctx, tx).Raw(
		`SELECT name, id FROM keys WHERE status = 'active' AND name = ANY($1)`,
		pq.Array(names)).Rows()
	if err != nil {
		return nil, fmt.Errorf("resolve key ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var id int64
		if err := rows.Scan(&name, &id); err != nil {
			return nil, fmt.Errorf("scan key id: %w", err)
		}
		out[name] = id
	}
	return out, rows.Err()
}

func (r *keyRepository) MaxSortIndex(ctx context.Context, tx *gorm.DB) (int64, error) {
	var max sql.NullInt64
	row := r.db(ctx, tx).Raw(`SELECT max(sort_index) FROM keys`).Row()
	if err := row.Scan(&max); err != nil {
		return 0, fmt.Errorf("max sort_index: %w", err)
	}
	return max.Int64, nil
}

func (r *keyRepository) CountActive(ctx context.Context, tx *gorm.DB) (int, error) {
	var n int
	row := r.db(ctx, tx).Raw(`SELECT count(*) FROM keys WHERE status = 'active'`).Row()
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count active keys: %w", err)
	}
	return n, nil
}

// isNoRows reports whether an error is the driver's empty-result signal.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no rows")
}
