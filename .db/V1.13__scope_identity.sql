-- Split identity from authorization, and scope the operational tables.
--
-- `users` keeps email and status: one person, one row, one identity. What
-- they may DO becomes per project, because a person may approve YouBiz copy
-- and have no business reading YouTrip's. The ordered-role trick survives
-- intact — viewer < editor < approver < admin still compares as one
-- inequality, it is simply read from this table now.
--
-- users.role is deliberately NOT dropped here. The middleware still reads it
-- until Plan 2 switches to per-project lookups; dropping it now would break
-- every authenticated request. Plan 2 drops it once nothing reads it.
--
-- One global privilege exists: a platform admin creates projects and grants
-- the first role in each. Without it the grant flow deadlocks on itself.
CREATE TABLE IF NOT EXISTS user_project_roles (
    email      CITEXT   NOT NULL REFERENCES users (email) ON DELETE CASCADE,
    project_id SMALLINT NOT NULL REFERENCES projects (id),
    role       TEXT     NOT NULL,
    granted_by CITEXT   NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_project_roles_pkey PRIMARY KEY (email, project_id),
    CONSTRAINT user_project_roles_role_check
        CHECK (role IN ('viewer', 'editor', 'approver', 'admin'))
);

CREATE INDEX IF NOT EXISTS idx_user_project_roles_project
    ON user_project_roles (project_id, role);

ALTER TABLE users ADD COLUMN IF NOT EXISTS is_platform_admin BOOLEAN NOT NULL DEFAULT false;

-- Every existing operator keeps exactly the access they had, on YouTrip.
INSERT INTO user_project_roles (email, project_id, role, granted_by)
SELECT email, 1, role, 'migration:V1.13' FROM users
ON CONFLICT (email, project_id) DO NOTHING;

-- Existing admins become platform admins: they are the people who must be
-- able to create the second project.
UPDATE users SET is_platform_admin = true WHERE role = 'admin';

-- A token belongs to one project, so YouBiz's CI cannot pull YouTrip's export.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE api_tokens SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE api_tokens
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT api_tokens_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

-- project_settings was named for exactly this and only ever held one project's.
ALTER TABLE project_settings ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE project_settings SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE project_settings
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT project_settings_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);
ALTER TABLE project_settings DROP CONSTRAINT project_settings_pkey;
ALTER TABLE project_settings ADD CONSTRAINT project_settings_pkey PRIMARY KEY (project_id, key);

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE audit_events SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE audit_events
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT audit_events_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

ALTER TABLE import_runs ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE import_runs SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE import_runs
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT import_runs_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id);

-- History keeps the V1.08 NO ACTION semantics: an audit record outlives what
-- it describes, so these composite keys must not cascade.
--
-- Constraint names verified against a live database before this DROP was
-- written: translation_history_key_id_fkey and key_history_key_id_fkey are
-- the names V1.08 left in place (it dropped and re-added them under the same
-- name, only changing ON DELETE CASCADE to NO ACTION).
-- translation_history_locale_id_fkey does exist — V1.01 declared it inline
-- and nothing has touched it since — so it is dropped here too, rather than
-- left behind as a stale single-column key sitting beside the new composite
-- one.
ALTER TABLE translation_history ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE translation_history SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE translation_history
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;
ALTER TABLE translation_history DROP CONSTRAINT translation_history_key_id_fkey;
ALTER TABLE translation_history DROP CONSTRAINT translation_history_locale_id_fkey;
ALTER TABLE translation_history
    ADD CONSTRAINT translation_history_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id),
    ADD CONSTRAINT translation_history_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE key_history ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_history SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_history
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;
ALTER TABLE key_history DROP CONSTRAINT key_history_key_id_fkey;
ALTER TABLE key_history
    ADD CONSTRAINT key_history_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id);
