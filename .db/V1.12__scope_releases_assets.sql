-- Scope releases and assets to a project.
--
-- RELEASE VERSIONS RESTART PER PROJECT. YouBiz's first release is 1, not
-- YouTrip's next number. Each app therefore has an independent version line,
-- which is what makes a per-project min_app_version floor meaningful.
--
-- ASSETS ARE ISOLATED RATHER THAN SHARED. Content addressing is preserved
-- within a project by putting the project in the S3 key prefix. Identical
-- bytes uploaded to two projects are stored twice — negligible for
-- screenshots, and it buys a permission model that is a column rather than a
-- join through key_assets, plus isolation at the storage layer.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE releases SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE releases
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT releases_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT releases_project_id_unique UNIQUE (project_id, id);

ALTER TABLE releases DROP CONSTRAINT releases_version_unique;
ALTER TABLE releases ADD CONSTRAINT releases_project_version_unique UNIQUE (project_id, version);

-- The OTA servable lookup is the hottest read in the service and now filters
-- on project first. Leading the index with project_id keeps it a LIMIT 1
-- index scan as projects accumulate.
DROP INDEX IF EXISTS idx_releases_servable;
CREATE INDEX IF NOT EXISTS idx_releases_servable
    ON releases (project_id, version DESC) WHERE rolled_back_at IS NULL;

ALTER TABLE release_bundles ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE release_bundles SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE release_bundles
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE release_bundles DROP CONSTRAINT release_bundles_release_id_fkey;
ALTER TABLE release_bundles DROP CONSTRAINT release_bundles_locale_id_fkey;
ALTER TABLE release_bundles
    ADD CONSTRAINT release_bundles_release_fkey
        FOREIGN KEY (project_id, release_id) REFERENCES releases (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT release_bundles_locale_fkey
        FOREIGN KEY (project_id, locale_id) REFERENCES locales (project_id, id);

ALTER TABLE assets ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE assets SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE assets
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT assets_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT assets_project_id_unique UNIQUE (project_id, id);

ALTER TABLE assets DROP CONSTRAINT assets_sha256_unique;
ALTER TABLE assets DROP CONSTRAINT assets_s3_key_unique;
ALTER TABLE assets
    ADD CONSTRAINT assets_project_sha256_unique UNIQUE (project_id, sha256),
    ADD CONSTRAINT assets_project_s3_key_unique UNIQUE (project_id, s3_key);

ALTER TABLE key_assets ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE key_assets SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE key_assets
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1;

ALTER TABLE key_assets DROP CONSTRAINT key_assets_key_id_fkey;
ALTER TABLE key_assets DROP CONSTRAINT key_assets_asset_id_fkey;
ALTER TABLE key_assets
    ADD CONSTRAINT key_assets_key_fkey
        FOREIGN KEY (project_id, key_id) REFERENCES keys (project_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT key_assets_asset_fkey
        FOREIGN KEY (project_id, asset_id) REFERENCES assets (project_id, id) ON DELETE CASCADE;
