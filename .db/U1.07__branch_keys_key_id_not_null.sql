-- Undo for V1.07. Never executed; see U1.00.
--
-- Restoring the nullable column restores the silent-drop class it was removed
-- to eliminate: applyKeyMetaSQL would once again skip a NULL-key_id delta and
-- report a successful merge. Treat V1.07 as forward-only in the strongest
-- sense — reverting it needs the merge code reverted with it.
DROP INDEX IF EXISTS idx_branch_keys_branch_key;
ALTER TABLE branch_keys ALTER COLUMN key_id DROP NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_branch_keys_branch_key
    ON branch_keys (branch_id, key_id) WHERE key_id IS NOT NULL;
