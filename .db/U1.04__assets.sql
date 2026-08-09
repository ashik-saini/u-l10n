-- Undo for V1.04. Never executed; see U1.00.
-- NOTE: dropping these rows does NOT delete the underlying S3 objects.
DROP INDEX IF EXISTS idx_key_assets_asset_id;
DROP TABLE IF EXISTS key_assets;
DROP TABLE IF EXISTS assets;
