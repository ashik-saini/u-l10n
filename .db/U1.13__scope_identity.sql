-- Undo for V1.13. Never executed; see U1.00.
--
-- Dropping user_project_roles discards every grant made after this migration
-- ran. Those grants cannot be reconstructed from users.role: by the time
-- anyone runs this undo, role has drifted away from what user_project_roles
-- holds — grants made, revoked or changed per project since V1.13 applied
-- have no other record. This is not a return to a previous working state; it
-- is a return to the single global role, minus every per-project change made
-- in between.
ALTER TABLE key_history DROP CONSTRAINT key_history_key_fkey;
ALTER TABLE key_history
    ADD CONSTRAINT key_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id);
ALTER TABLE key_history DROP COLUMN project_id;

ALTER TABLE translation_history
    DROP CONSTRAINT translation_history_key_fkey,
    DROP CONSTRAINT translation_history_locale_fkey;
ALTER TABLE translation_history
    ADD CONSTRAINT translation_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id),
    ADD CONSTRAINT translation_history_locale_id_fkey
        FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE translation_history DROP COLUMN project_id;

ALTER TABLE import_runs DROP CONSTRAINT import_runs_project_fkey;
ALTER TABLE import_runs DROP COLUMN project_id;

ALTER TABLE audit_events DROP CONSTRAINT audit_events_project_fkey;
ALTER TABLE audit_events DROP COLUMN project_id;

ALTER TABLE project_settings DROP CONSTRAINT project_settings_pkey;
ALTER TABLE project_settings ADD CONSTRAINT project_settings_pkey PRIMARY KEY (key);
ALTER TABLE project_settings DROP CONSTRAINT project_settings_project_fkey;
ALTER TABLE project_settings DROP COLUMN project_id;

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_project_fkey;
ALTER TABLE api_tokens DROP COLUMN project_id;

UPDATE users SET is_platform_admin = false;
ALTER TABLE users DROP COLUMN is_platform_admin;

DROP TABLE user_project_roles;
