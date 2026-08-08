-- Undo for V1.06. Never executed; see U1.00.
DROP TABLE IF EXISTS project_settings;
DROP INDEX IF EXISTS idx_import_runs_started_at;
DROP TABLE IF EXISTS import_runs;
DROP INDEX IF EXISTS idx_audit_events_target;
DROP INDEX IF EXISTS idx_audit_events_actor;
DROP INDEX IF EXISTS idx_audit_events_created_at;
DROP TABLE IF EXISTS audit_events;
