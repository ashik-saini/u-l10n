-- Undo for V1.10. Never executed; see U1.00.
--
-- Reverting drops the composite foreign keys, which are the only thing
-- preventing a translation from pairing one project's key with another
-- project's locale. With more than one project present that is immediate
-- silent corruption, not a return to a previous working state.
ALTER TABLE key_tags DROP CONSTRAINT key_tags_key_fkey, DROP CONSTRAINT key_tags_tag_fkey;
ALTER TABLE key_tags
    ADD CONSTRAINT key_tags_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT key_tags_tag_id_fkey FOREIGN KEY (tag_id) REFERENCES tags (id) ON DELETE CASCADE;
ALTER TABLE key_tags DROP COLUMN project_id;

ALTER TABLE translations DROP CONSTRAINT translations_key_fkey, DROP CONSTRAINT translations_locale_fkey;
ALTER TABLE translations
    ADD CONSTRAINT translations_key_id_fkey FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE,
    ADD CONSTRAINT translations_locale_id_fkey FOREIGN KEY (locale_id) REFERENCES locales (id);
ALTER TABLE translations DROP COLUMN project_id;

ALTER TABLE tags DROP CONSTRAINT tags_project_name_unique;
ALTER TABLE tags ADD CONSTRAINT tags_name_unique UNIQUE (name);
ALTER TABLE tags DROP COLUMN project_id;

DROP INDEX IF EXISTS idx_keys_name_active;
CREATE UNIQUE INDEX idx_keys_name_active ON keys (name) WHERE status = 'active';
ALTER TABLE keys DROP CONSTRAINT keys_project_lokalise_key_unique;
ALTER TABLE keys ADD CONSTRAINT keys_lokalise_key_id_unique UNIQUE (lokalise_key_id);
ALTER TABLE keys DROP COLUMN project_id;

ALTER TABLE locales DROP CONSTRAINT locales_project_code_unique;
ALTER TABLE locales ADD CONSTRAINT locales_code_unique UNIQUE (code);
ALTER TABLE locales DROP COLUMN project_id, DROP COLUMN status;
