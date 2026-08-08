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

	// ServableBundle returns what an OTA client should receive for a locale.
	ServableBundle(ctx context.Context, tx *gorm.DB, localeID int16, appVersion string) (ServableBundle, error)
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

// ServableBundle is one locale's bundle from the newest eligible release.
type ServableBundle struct {
	ReleaseVersion int64
	Strings        []byte
	SHA256         string
	// KillSwitched reports that the newest release for this locale has been
	// rolled back and no earlier one is eligible.
	KillSwitched bool
}

// servableBundleSQL returns the newest release that is eligible for a client.
//
// Eligibility has two parts, both expressed in SQL so no caller can forget one:
//
//	rolled_back_at IS NULL     the kill switch
//	min_app_version <= client  the semver floor
//
// The floor is compared as an INTEGER TRIPLE, not as text: '4.9.0' > '4.10.0'
// lexically, which would withhold a release from exactly the clients it was
// meant for. NULL means every client is eligible.
const servableBundleSQL = `
WITH client AS (
    SELECT COALESCE(NULLIF($2, ''), '0.0.0') AS v
)
SELECT r.version, rb.strings::text, rb.sha256
  FROM releases r
  JOIN release_bundles rb ON rb.release_id = r.id
 WHERE rb.locale_id = $1
   AND r.rolled_back_at IS NULL
   AND (
        r.min_app_version IS NULL
     OR string_to_array(r.min_app_version, '.')::int[]
        <= string_to_array((SELECT v FROM client), '.')::int[]
   )
 ORDER BY r.version DESC
 LIMIT 1`

// ServableBundle returns what an OTA client should receive.
//
// appVersion may be empty, which is treated as 0.0.0 — the most conservative
// reading, so a client that omits the header only ever receives releases with
// no floor at all.
func (r *releaseRepository) ServableBundle(
	ctx context.Context, tx *gorm.DB, localeID int16, appVersion string,
) (ServableBundle, error) {
	var b ServableBundle

	row := r.db(ctx, tx).Raw(servableBundleSQL, localeID, appVersion).Row()
	switch err := row.Scan(&b.ReleaseVersion, &b.Strings, &b.SHA256); {
	case err == nil:
		return b, nil
	case isNoRows(err):
		// Nothing eligible. Distinguish "rolled back" from "never released":
		// the first tells a client to clear its cache, the second is simply a
		// service with no releases yet.
		var anyRolledBack bool
		row = r.db(ctx, tx).Raw(`
			SELECT EXISTS (
			    SELECT 1 FROM releases r
			      JOIN release_bundles rb ON rb.release_id = r.id
			     WHERE rb.locale_id = $1 AND r.rolled_back_at IS NOT NULL)`, localeID).Row()
		if scanErr := row.Scan(&anyRolledBack); scanErr != nil {
			return b, fmt.Errorf("check rollback state: %w", scanErr)
		}
		b.KillSwitched = anyRolledBack
		return b, ErrNotFound
	default:
		return b, fmt.Errorf("read servable bundle: %w", err)
	}
}
