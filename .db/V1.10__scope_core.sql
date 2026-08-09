-- Scope the core matrix to a project.
--
-- THE COMPOSITE FOREIGN KEYS ARE THE POINT. A single-column key
-- (translations.key_id -> keys.id) cannot express "and they must belong to
-- the same project", so nothing but a reviewer's attention would stop a
-- YouTrip key being paired with a YouBiz locale. Since locale SETS now differ
-- per project, that pairing is not hypothetical. Referencing
-- keys (project_id, id) makes the illegal row unrepresentable.
--
-- THE TEMPORARY DEFAULT. Every column below is created with DEFAULT 1 — the
-- YouTrip project seeded in V1.09 — so that today's INSERT statements, which
-- know nothing about projects, keep working. Plan 2 passes the scope
-- explicitly and drops these defaults; until then the default is what keeps
-- the service running.
ALTER TABLE locales ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE locales SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE locales
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT locales_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT locales_project_id_unique UNIQUE (project_id, id);

-- The code is unique WITHIN a project now: YouBiz may ship its own en-SG.
ALTER TABLE locales DROP CONSTRAINT locales_code_unique;
ALTER TABLE locales ADD CONSTRAINT locales_project_code_unique UNIQUE (project_id, code);

-- Two locales in one project must not claim the same export directory.
-- Without this an admin could aim both at `values/` and the export zip would
-- silently write one over the other — a data-integrity failure the database
-- should refuse rather than a reviewer catch.
ALTER TABLE locales
    ADD CONSTRAINT locales_project_flutter_dir_unique UNIQUE (project_id, flutter_dir),
    ADD CONSTRAINT locales_project_android_dir_unique UNIQUE (project_id, android_values_dir),
    ADD CONSTRAINT locales_project_ios_lproj_unique   UNIQUE (project_id, ios_lproj);

-- Locales become admin-managed data rather than migration-seeded reference
-- data, so they need a lifecycle. Archived, never deleted: translations and
-- history reference them.
ALTER TABLE locales ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE locales ADD CONSTRAINT locales_status_check CHECK (status IN ('active', 'archived'));

ALTER TABLE keys ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE keys SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE keys
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT keys_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT keys_project_id_unique UNIQUE (project_id, id);

-- Name uniqueness among ACTIVE keys becomes per project. The partial
-- predicate is unchanged: a soft-deleted key must not block reuse of its name.
DROP INDEX IF EXISTS idx_keys_name_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_keys_name_active
    ON keys (project_id, name) WHERE status = 'active';

-- The import anchor is per project: each project imports from its own
-- Lokalise project, whose key ids are its own numbering.
ALTER TABLE keys DROP CONSTRAINT keys_lokalise_key_id_unique;
ALTER TABLE keys ADD CONSTRAINT keys_project_lokalise_key_unique UNIQUE (project_id, lokalise_key_id);

ALTER TABLE translations ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE translations SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE translations
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE translations DROP CONSTRAINT translations_key_id_fkey;
ALTER TABLE translations DROP CONSTRAINT translations_locale_id_fkey;
ALTER TABLE translations
    ADD CONSTRAINT translations_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT translations_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE tags ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE tags SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE tags
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT tags_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT tags_project_id_unique UNIQUE (project_id, id);

ALTER TABLE tags DROP CONSTRAINT tags_name_unique;
ALTER TABLE tags ADD CONSTRAINT tags_project_name_unique UNIQUE (project_id, name);

ALTER TABLE key_tags ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_tags SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_tags
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE key_tags DROP CONSTRAINT key_tags_key_id_fkey;
ALTER TABLE key_tags DROP CONSTRAINT key_tags_tag_id_fkey;
ALTER TABLE key_tags
    ADD CONSTRAINT key_tags_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT key_tags_tag_fkey
        FOREIGN KEY (project_id, tag_id) REFERENCES tags (project_id, id) ON DELETE CASCADE;

-- The key browse is the portal's busiest query and now filters on project
-- first. Leading the index with project_id keeps it a range scan rather than
-- a filter over every project's keys.
DROP INDEX IF EXISTS idx_keys_status_name;
CREATE INDEX IF NOT EXISTS idx_keys_project_status_name ON keys (project_id, status, name);
