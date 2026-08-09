package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// Asset is one uploaded context screenshot.
//
// Immutable by construction: the row is keyed on the sha256 of the bytes, so
// "editing" an asset is impossible — different bytes are a different asset.
type Asset struct {
	ID          int64
	S3Key       string
	SHA256      string
	Filename    string
	ContentType string
	Bytes       int
	Width       *int
	Height      *int
	UploadedBy  string
	CreatedAt   time.Time
}

// AssetRepository owns the assets and key_assets tables.
//
// Every method takes an optional tx. Asset creation and its audit row, and
// attach/detach and their audit rows, must land together or not at all — see
// base.db for why a repository must never open the transaction itself.
type AssetRepository interface {
	// BySHA256 resolves an asset by content hash. This is the dedupe lookup:
	// identical bytes are never stored, and never uploaded, twice.
	BySHA256(ctx context.Context, tx *gorm.DB, sha256 string) (Asset, error)

	ByID(ctx context.Context, tx *gorm.DB, id int64) (Asset, error)

	// Create inserts an asset, or returns the existing row if another request
	// confirmed the same bytes first. Never returns a duplicate-key error: two
	// concurrent confirms of one screenshot are a race, not a caller mistake.
	Create(ctx context.Context, tx *gorm.DB, a Asset) (Asset, error)

	// Attach links an asset to a key, replacing the note if the link exists.
	Attach(ctx context.Context, tx *gorm.DB, keyID, assetID int64, note, createdBy string) error

	// Detach removes the link. Returns ErrNotFound when there was none, so a
	// caller can tell "unlinked it" from "there was nothing to unlink".
	Detach(ctx context.Context, tx *gorm.DB, keyID, assetID int64) error

	// KeyExists reports whether an active key exists.
	//
	// Checked before an attach so a bad key id is a 404 rather than a foreign
	// key violation surfacing as a 500.
	KeyExists(ctx context.Context, tx *gorm.DB, keyID int64) (bool, error)
}

type assetRepository struct{ base }

func ProvideAssetRepository(connector database.GORMConnector) AssetRepository {
	return &assetRepository{base{connector: connector}}
}

const selectAssetColumns = `
    id, s3_key, sha256, filename, content_type, bytes, width, height,
    uploaded_by, created_at`

func scanAsset(row *sql.Row) (Asset, error) {
	var (
		a             Asset
		width, height sql.NullInt64
	)
	err := row.Scan(&a.ID, &a.S3Key, &a.SHA256, &a.Filename, &a.ContentType,
		&a.Bytes, &width, &height, &a.UploadedBy, &a.CreatedAt)
	if err != nil {
		return a, err
	}
	if width.Valid {
		w := int(width.Int64)
		a.Width = &w
	}
	if height.Valid {
		h := int(height.Int64)
		a.Height = &h
	}
	return a, nil
}

func (r *assetRepository) BySHA256(ctx context.Context, tx *gorm.DB, sha256 string) (Asset, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+selectAssetColumns+` FROM assets WHERE sha256 = $1`, sha256).Row()

	a, err := scanAsset(row)
	switch {
	case err == nil:
		return a, nil
	case isNoRows(err):
		return a, ErrNotFound
	default:
		return a, fmt.Errorf("read asset by sha256: %w", err)
	}
}

func (r *assetRepository) ByID(ctx context.Context, tx *gorm.DB, id int64) (Asset, error) {
	row := r.db(ctx, tx).Raw(
		`SELECT `+selectAssetColumns+` FROM assets WHERE id = $1`, id).Row()

	a, err := scanAsset(row)
	switch {
	case err == nil:
		return a, nil
	case isNoRows(err):
		return a, ErrNotFound
	default:
		return a, fmt.Errorf("read asset %d: %w", id, err)
	}
}

// createAssetSQL inserts, or yields the winner of a race.
//
// Hand-written because GORM v1 has no clause.OnConflict — that is a v2 API, and
// it appears nowhere in this codebase.
//
// DO NOTHING rather than DO UPDATE: an asset is its content, so there is
// nothing an existing row could usefully be updated to. DO NOTHING returns no
// row, hence the read-back below — the same shape as UpsertByName.
//
// The conflict target is (project_id, sha256), not sha256 alone: V1.12
// dropped the global assets_sha256_unique in favour of
// assets_project_sha256_unique, because two projects are now allowed to hold
// the same image. This INSERT never names project_id, so every row it writes
// still takes the column's DEFAULT 1 until a caller passes the scope
// explicitly — see TODO(plan-2) at the other scoped call sites.
const createAssetSQL = `
INSERT INTO assets (s3_key, sha256, filename, content_type, bytes, width, height, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (project_id, sha256) DO NOTHING
RETURNING ` + selectAssetColumns

func (r *assetRepository) Create(ctx context.Context, tx *gorm.DB, a Asset) (Asset, error) {
	db := r.db(ctx, tx)

	row := db.Raw(createAssetSQL, a.S3Key, a.SHA256, a.Filename, a.ContentType,
		a.Bytes, a.Width, a.Height, a.UploadedBy).Row()

	created, err := scanAsset(row)
	switch {
	case err == nil:
		return created, nil

	case isNoRows(err):
		// Another request confirmed the same bytes between our dedupe lookup and
		// this insert. Its row is as good as ours would have been, so return it
		// rather than failing a request that asked for nothing unreasonable.
		return r.BySHA256(ctx, tx, a.SHA256)

	default:
		return a, fmt.Errorf("create asset %s: %w", a.SHA256, err)
	}
}

// attachSQL links a key and an asset.
//
// ON CONFLICT DO UPDATE on the note, so re-attaching with better guidance
// amends it instead of failing. sort_order appends: a key's screenshots are
// shown in the order they were attached.
const attachSQL = `
INSERT INTO key_assets (key_id, asset_id, note, sort_order, created_by)
VALUES ($1, $2, $3,
        COALESCE((SELECT max(sort_order) + 1 FROM key_assets WHERE key_id = $1), 0),
        $4)
ON CONFLICT (key_id, asset_id) DO UPDATE SET note = EXCLUDED.note`

func (r *assetRepository) Attach(
	ctx context.Context, tx *gorm.DB, keyID, assetID int64, note, createdBy string,
) error {
	err := r.db(ctx, tx).Exec(attachSQL, keyID, assetID, note, createdBy).Error
	if err != nil {
		return fmt.Errorf("attach asset %d to key %d: %w", assetID, keyID, err)
	}
	return nil
}

func (r *assetRepository) Detach(ctx context.Context, tx *gorm.DB, keyID, assetID int64) error {
	res := r.db(ctx, tx).Exec(
		`DELETE FROM key_assets WHERE key_id = $1 AND asset_id = $2`, keyID, assetID)
	if res.Error != nil {
		return fmt.Errorf("detach asset %d from key %d: %w", assetID, keyID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("asset %d is not attached to key %d: %w", assetID, keyID, ErrNotFound)
	}
	return nil
}

func (r *assetRepository) KeyExists(ctx context.Context, tx *gorm.DB, keyID int64) (bool, error) {
	var exists bool
	row := r.db(ctx, tx).Raw(
		`SELECT EXISTS (SELECT 1 FROM keys WHERE id = $1 AND status = 'active')`, keyID).Row()
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("check key %d exists: %w", keyID, err)
	}
	return exists, nil
}
