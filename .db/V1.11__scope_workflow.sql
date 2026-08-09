-- Scope branches and merge requests to a project.
--
-- Branch names become unique WITHIN a project, so both teams may run a
-- `q3-copy` without one blocking the other. The branch delta tables reference
-- keys and locales through composite keys for the same reason V1.10 gave: a
-- delta must not name another project's key.
--
-- merge_requests carries project_id although it could be reached through its
-- branch. The merge transaction filters on it directly and the advisory lock
-- is derived from it; a join on every one of those statements would be cost
-- with no benefit.
ALTER TABLE branches ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branches SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branches
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT branches_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT branches_project_id_unique UNIQUE (project_id, id);

ALTER TABLE branches DROP CONSTRAINT branches_name_unique;
ALTER TABLE branches ADD CONSTRAINT branches_project_name_unique UNIQUE (project_id, name);

ALTER TABLE branch_translations ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branch_translations SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branch_translations
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_branch_id_fkey;
ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_key_id_fkey;
ALTER TABLE branch_translations DROP CONSTRAINT branch_translations_locale_id_fkey;
ALTER TABLE branch_translations
    ADD CONSTRAINT branch_translations_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE branch_keys ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE branch_keys SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE branch_keys
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE branch_keys DROP CONSTRAINT branch_keys_branch_id_fkey;
ALTER TABLE branch_keys DROP CONSTRAINT branch_keys_key_id_fkey;
ALTER TABLE branch_keys
    ADD CONSTRAINT branch_keys_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_keys_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE;

ALTER TABLE merge_requests ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE merge_requests SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE merge_requests
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE merge_requests DROP CONSTRAINT merge_requests_branch_id_fkey;
ALTER TABLE merge_requests
    ADD CONSTRAINT merge_requests_branch_fkey
        FOREIGN KEY (project_id, branch_id) REFERENCES branches (project_id, id) ON DELETE CASCADE;

ALTER TABLE merge_conflict_resolutions ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE merge_conflict_resolutions SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE merge_conflict_resolutions
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE merge_conflict_resolutions DROP CONSTRAINT merge_conflict_resolutions_key_id_fkey;
ALTER TABLE merge_conflict_resolutions
    ADD CONSTRAINT merge_conflict_resolutions_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE;

-- locale_id is NULL for a key-metadata conflict, which has no locale
-- dimension (see V1.02). A composite key still closes the pairing gap for the
-- rows that DO carry one: FOREIGN KEY (project_id, locale_id) uses Postgres's
-- default MATCH SIMPLE, which skips enforcement only when locale_id itself is
-- NULL — project_id is NOT NULL on this table, so a metadata-conflict row
-- (locale_id NULL) still inserts freely, while a value-conflict row naming a
-- real locale from another project is refused exactly like every other
-- pairing this migration closes.
ALTER TABLE merge_conflict_resolutions DROP CONSTRAINT merge_conflict_resolutions_locale_id_fkey;
ALTER TABLE merge_conflict_resolutions
    ADD CONSTRAINT merge_conflict_resolutions_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);
