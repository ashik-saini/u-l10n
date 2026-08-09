-- Undo for V1.12. Never executed; see U1.00.
--
-- Reverting drops the composite foreign keys that are the only thing
-- preventing a release_bundle or a key_asset from pairing one project's
-- release/key with another project's locale/asset. With more than one
-- project present that is immediate silent corruption, not a return to a
-- previous working state.
--
-- Restoring the global `assets_sha256_unique` will FAIL OUTRIGHT the moment
-- two projects hold an asset with the same content hash — which, once a
-- second project exists, is not a hypothetical: two products commonly reuse
-- the same icon or screenshot. That failure is intended. The alternative is
-- letting the ADD CONSTRAINT silently succeed by deleting or merging one
-- project's row into the other's, which is exactly the kind of quiet data
-- loss this undo file must not perform. If this migration is ever actually
-- run, the operator must resolve the sha256 collision by hand, in the open,
-- rather than have this script guess which project's asset survives.
--
-- Restoring `assets_s3_key_unique` fails for the identical reason, today
-- unconditionally rather than only once collision is possible:
-- assetsvc.s3Key() derives the key from sha256 alone, with no project
-- component (see the TODO(plan-2) there and V1.12's header). Two projects
-- holding the same bytes already produce two `assets` rows with the SAME
-- s3_key, distinguished only by this migration's project-scoped unique index
-- — restoring the global one hits that duplicate immediately.
ALTER TABLE key_assets DROP CONSTRAINT key_assets_key_fkey, DROP CONSTRAINT key_assets_asset_fkey;
ALTER TABLE key_assets
    ADD CONSTRAINT key_assets_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT key_assets_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES assets (id) ON DELETE CASCADE;
ALTER TABLE key_assets DROP COLUMN project_id;

ALTER TABLE assets DROP CONSTRAINT assets_project_sha256_unique, DROP CONSTRAINT assets_project_s3_key_unique;
ALTER TABLE assets
    ADD CONSTRAINT assets_sha256_unique UNIQUE (sha256),
    ADD CONSTRAINT assets_s3_key_unique UNIQUE (s3_key);
ALTER TABLE assets DROP CONSTRAINT assets_project_id_unique, DROP CONSTRAINT assets_project_fkey;
ALTER TABLE assets DROP COLUMN project_id;

ALTER TABLE release_bundles DROP CONSTRAINT release_bundles_release_fkey, DROP CONSTRAINT release_bundles_locale_fkey;
ALTER TABLE release_bundles
    ADD CONSTRAINT release_bundles_release_id_fkey FOREIGN KEY (release_id) REFERENCES releases (id) ON DELETE CASCADE,
    ADD CONSTRAINT release_bundles_locale_id_fkey FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE release_bundles DROP COLUMN project_id;

DROP INDEX IF EXISTS idx_releases_servable;
CREATE INDEX idx_releases_servable ON releases (version DESC) WHERE rolled_back_at IS NULL;

ALTER TABLE releases DROP CONSTRAINT releases_project_version_unique;
ALTER TABLE releases ADD CONSTRAINT releases_version_unique UNIQUE (version);
ALTER TABLE releases DROP CONSTRAINT releases_project_id_unique, DROP CONSTRAINT releases_project_fkey;
ALTER TABLE releases DROP COLUMN project_id;
