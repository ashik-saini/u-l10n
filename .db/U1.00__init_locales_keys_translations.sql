-- Undo for V1.00. NOTE: never executed — Flyway Community cannot run `undo`,
-- and no script in the deployment pipeline invokes it. Kept for parity with
-- the other services and as documentation of what the forward migration owns.
DROP INDEX IF EXISTS idx_translations_locale_id;
DROP TABLE IF EXISTS translations;
DROP INDEX IF EXISTS idx_keys_platforms;
DROP INDEX IF EXISTS idx_keys_sort_index;
DROP INDEX IF EXISTS idx_keys_name_active;
DROP TABLE IF EXISTS keys;
DROP TABLE IF EXISTS locales;
-- citext is intentionally NOT dropped: other migrations depend on it.
