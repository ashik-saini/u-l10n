-- Undo for V1.01. Never executed; see U1.00.
DROP INDEX IF EXISTS idx_key_history_key_id;
DROP TABLE IF EXISTS key_history;
DROP INDEX IF EXISTS idx_translation_history_key_locale;
DROP TABLE IF EXISTS translation_history;
DROP INDEX IF EXISTS idx_key_tags_tag_id;
DROP TABLE IF EXISTS key_tags;
DROP TABLE IF EXISTS tags;
