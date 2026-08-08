package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// Release is an immutable, numbered snapshot of master.
type Release struct {
	ID      int64
	Version int64
	Source  string
}

// ReleaseRepository creates releases and materialises their bundles.
type ReleaseRepository interface {
	// Create allocates the next version and inserts the release.
	Create(ctx context.Context, tx *gorm.DB, source string, mergeRequestID *int64, createdBy string) (Release, error)

	// MaterialiseBundle writes one locale's flat key/value map for a release,
	// with its sha256 — which doubles as the HTTP ETag on the OTA path.
	MaterialiseBundle(ctx context.Context, tx *gorm.DB, releaseID int64, locale model.Locale, rows []ExportRow) error

	BundleSHA(ctx context.Context, tx *gorm.DB, releaseID int64, localeID int16) (string, error)
}

type releaseRepository struct{ base }

func ProvideReleaseRepository(connector database.GORMConnector) ReleaseRepository {
	return &releaseRepository{base{connector: connector}}
}

// Version is allocated as max+1 inside the merge transaction, under the same
// advisory lock that serialises merges — so two concurrent merges cannot pick
// the same number. A sequence would also work, but would leave gaps on
// rollback, and release numbers are read by humans.
//
// A globally monotonic counter is only safe because u-l10n runs as a single
// global instance; under per-market deployment this would need a composite
// (market, version) key.
const createReleaseSQL = `
INSERT INTO releases (version, source, merge_request_id, created_by)
VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, $1, $2, $3)
RETURNING id, version, source`

func (r *releaseRepository) Create(
	ctx context.Context, tx *gorm.DB, source string, mergeRequestID *int64, createdBy string,
) (Release, error) {
	var rel Release
	row := r.db(ctx, tx).Raw(createReleaseSQL, source, mergeRequestID, createdBy).Row()
	if err := row.Scan(&rel.ID, &rel.Version, &rel.Source); err != nil {
		return rel, fmt.Errorf("create release: %w", err)
	}
	return rel, nil
}

func (r *releaseRepository) MaterialiseBundle(
	ctx context.Context, tx *gorm.DB, releaseID int64, locale model.Locale, rows []ExportRow,
) error {
	// The bundle is the flutter key set with empty values INCLUDED — exactly
	// what assets/langs/<locale>.json holds today, so the OTA payload and the
	// asset bundled in the app binary are structurally identical and cannot
	// drift apart.
	strings := make(map[string]string, len(rows))
	for _, row := range rows {
		strings[row.Key] = row.Value
	}

	// Marshal with sorted keys (encoding/json sorts map keys) so the same
	// content always produces the same bytes and therefore the same sha256.
	// A fingerprint that changed without the content changing would break
	// every client's ETag on every merge.
	payload, err := json.Marshal(strings)
	if err != nil {
		return fmt.Errorf("marshal bundle for %s: %w", locale.Code, err)
	}
	sum := sha256.Sum256(payload)

	err = r.db(ctx, tx).Exec(`
		INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		releaseID, locale.ID, string(payload), hex.EncodeToString(sum[:]),
		len(strings), len(payload)).Error
	if err != nil {
		return fmt.Errorf("materialise bundle for %s: %w", locale.Code, err)
	}
	return nil
}

func (r *releaseRepository) BundleSHA(
	ctx context.Context, tx *gorm.DB, releaseID int64, localeID int16,
) (string, error) {
	var sha string
	row := r.db(ctx, tx).Raw(
		`SELECT sha256 FROM release_bundles WHERE release_id = $1 AND locale_id = $2`,
		releaseID, localeID).Row()
	if err := row.Scan(&sha); err != nil {
		return "", fmt.Errorf("read bundle sha: %w", err)
	}
	return sha, nil
}
