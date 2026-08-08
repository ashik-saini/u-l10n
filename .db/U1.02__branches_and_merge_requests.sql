-- Undo for V1.02. Never executed; see U1.00.
DROP INDEX IF EXISTS idx_merge_conflict_resolutions_unique;
DROP TABLE IF EXISTS merge_conflict_resolutions;
DROP INDEX IF EXISTS idx_merge_request_events_mr;
DROP TABLE IF EXISTS merge_request_events;
DROP INDEX IF EXISTS idx_merge_requests_one_live_per_branch;
DROP TABLE IF EXISTS merge_requests;
DROP INDEX IF EXISTS idx_branch_keys_branch_name;
DROP INDEX IF EXISTS idx_branch_keys_branch_key;
DROP TABLE IF EXISTS branch_keys;
DROP INDEX IF EXISTS idx_branch_translations_key_locale;
DROP TABLE IF EXISTS branch_translations;
DROP TABLE IF EXISTS branches;
