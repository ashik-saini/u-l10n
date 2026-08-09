-- Scope releases and assets to a project.
--
-- RELEASE VERSIONS RESTART PER PROJECT. YouBiz's first release is 1, not
-- YouTrip's next number. Each app therefore has an independent version line,
-- which is what makes a per-project min_app_version floor meaningful.
--
-- ASSETS ARE MEANT TO BE ISOLATED RATHER THAN SHARED, BUT THE STORAGE LAYER
-- DOES NOT YET DO ITS PART. The design is: identical bytes uploaded to two
-- projects are stored twice, by putting the project in the S3 key prefix —
-- negligible cost for screenshots, and it buys a permission model that is a
-- column rather than a join through key_assets, plus isolation at the storage
-- layer. assetsvc.s3Key() does not carry a project component yet, so two
-- projects uploading identical bytes today produce two `assets` rows (correctly
-- distinguished by this migration's project-scoped uniques) that both compute
-- the SAME s3_key and therefore point at ONE S3 object — deleting one
-- project's asset would delete the other's. Prefixing s3Key() belongs with the
-- rest of the explicit-scope work in the next plan (see the matching
-- TODO(plan-2) at assetsvc.s3Key and at AssetRepository.BySHA256, which has the
-- same gap on the read side).
ALTER TABLE releases ADD COLUMN IF NOT EXISTS project_id SMALLINT;
UPDATE releases SET project_id = 1 WHERE project_id IS NULL;
ALTER TABLE releases
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN project_id SET DEFAULT 1,
    ADD CONSTRAINT releases_project_fkey FOREIGN KEY (project_id) REFERENCES projects (id),
    ADD CONSTRAINT releases_project_id_unique UNIQUE (project_id, id);

ALTER TABLE releases DROP CONSTRAINT releases_version_unique;
ALTER TABLE releases ADD CONSTRAINT releases_project_version_unique UNIQUE (project_id, version);

-- The OTA servable lookup is the hottest read in the service. Leading the
-- index with project_id is necessary but NOT sufficient on its own: an index
-- led by a column the query never filters on cannot satisfy
-- `ORDER BY version DESC` once a second project's rows are interleaved with
-- the first's. pkg/repository/release.go's servableBundleSQL carries a
-- matching `r.project_id = (SELECT project_id FROM locales WHERE id = $1)`
-- predicate for exactly this reason — see the comment there for the measured
-- query plan before and after.
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
