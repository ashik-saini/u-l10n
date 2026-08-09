-- Undo for V1.11. Never executed; see U1.00.
--
-- Reverting drops the composite foreign keys that are the only thing
-- stopping a branch delta or a merge request from naming another project's
-- key, locale or branch. With more than one project present that is
-- immediate silent corruption, not a return to a previous working state —
-- exactly the case U1.10 already made for the core matrix, now true of the
-- branch workflow too.
ALTER TABLE merge_conflict_resolutions DROP CONSTRAINT merge_conflict_resolutions_locale_fkey;
ALTER TABLE merge_conflict_resolutions
    ADD CONSTRAINT merge_conflict_resolutions_locale_id_fkey
        FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE merge_conflict_resolutions DROP CONSTRAINT merge_conflict_resolutions_key_fkey;
ALTER TABLE merge_conflict_resolutions
    ADD CONSTRAINT merge_conflict_resolutions_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE;
ALTER TABLE merge_conflict_resolutions DROP COLUMN project_id;

ALTER TABLE merge_requests DROP CONSTRAINT merge_requests_branch_fkey;
ALTER TABLE merge_requests
    ADD CONSTRAINT merge_requests_branch_id_fkey
        FOREIGN KEY (branch_id) REFERENCES branches (id) ON DELETE CASCADE;
ALTER TABLE merge_requests DROP COLUMN project_id;

ALTER TABLE branch_keys DROP CONSTRAINT branch_keys_branch_fkey, DROP CONSTRAINT branch_keys_key_fkey;
ALTER TABLE branch_keys
    ADD CONSTRAINT branch_keys_branch_id_fkey FOREIGN KEY (branch_id) REFERENCES branches (id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_keys_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE;
ALTER TABLE branch_keys DROP COLUMN project_id;

ALTER TABLE branch_translations
    DROP CONSTRAINT branch_translations_branch_fkey,
    DROP CONSTRAINT branch_translations_key_fkey,
    DROP CONSTRAINT branch_translations_locale_fkey;
ALTER TABLE branch_translations
    ADD CONSTRAINT branch_translations_branch_id_fkey FOREIGN KEY (branch_id) REFERENCES branches (id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT branch_translations_locale_id_fkey FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE branch_translations DROP COLUMN project_id;

ALTER TABLE branches DROP CONSTRAINT branches_project_name_unique;
ALTER TABLE branches ADD CONSTRAINT branches_name_unique UNIQUE (name);
ALTER TABLE branches
    DROP CONSTRAINT branches_project_id_unique,
    DROP CONSTRAINT branches_project_fkey;
ALTER TABLE branches DROP COLUMN project_id;
