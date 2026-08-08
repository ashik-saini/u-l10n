-- branch_keys.key_id becomes NOT NULL.
--
-- WHY. V1.02 allowed NULL to mean "a key created on this branch that does not
-- yet exist on master". Nothing could ever usefully occupy that state, and the
-- merge silently dropped it:
--
--   * branch_translations.key_id is NOT NULL REFERENCES keys (id), so a
--     branch-created key could not carry a single VALUE until a real keys row
--     existed. A NULL-key_id delta was therefore metadata with nothing attached
--     — a key nobody could translate.
--   * applyKeyMetaSQL folds metadata into master with `bk.key_id = k.id`. A
--     NULL matched no master row, the UPDATE skipped it, and the merge reported
--     success having never landed the key. Silent, successful-looking data loss.
--
-- THE REPLACEMENT MODEL. A key created on a branch is INSERTed into `keys`
-- immediately with status = 'draft', and the branch carries an ordinary
-- branch_keys delta holding that key_id with status = 'active'. That is enough
-- to make the impossible state impossible rather than merely handled:
--
--   * Drafts reach nobody. forExportSQL filters `k.status = 'active'`, and
--     every release bundle — including the OTA bundle — is materialised from
--     exactly those rows, so a draft is invisible to the export endpoint, to
--     the OTA endpoint and to every generated mobile file.
--   * The merge promotes draft -> active through the EXISTING metadata path,
--     which already assigns `status = bk.status`. No new branch in the merge,
--     no second way for a key to reach master.
--   * branch_translations' own foreign key is now the thing enforcing
--     correctness: a value can only be written against a key that exists.
--
-- NO DATA MIGRATION ACCOMPANIES THIS, deliberately. No code path has ever
-- written a NULL key_id — setKeyMetaSQL has only ever been called with an
-- existing key's id, and the service refused key creation on a branch outright
-- — so this ALTER is expected to find nothing. If it does find something, it
-- fails the migration and a human looks at it. Quietly DELETEing rows whose
-- provenance nobody can explain would be the same silent loss this migration
-- exists to end.
ALTER TABLE branch_keys ALTER COLUMN key_id SET NOT NULL;

-- The partial predicate existed only because NULLs do not collide in a plain
-- unique index. With key_id NOT NULL it is noise that every writer has to
-- repeat in its ON CONFLICT target for inference to match the index.
DROP INDEX IF EXISTS idx_branch_keys_branch_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_branch_keys_branch_key
    ON branch_keys (branch_id, key_id);
