-- Undo for V1.08. Never executed; see U1.00.
--
-- Restoring ON DELETE CASCADE restores the silent-loss class V1.08 removed: a
-- hand-typed DELETE of a keys row would once again take the key's entire audit
-- trail with it, unremarked — in exactly the situation where "who changed this
-- and when" is the question being asked. If a hard delete is ever genuinely
-- required, delete the history rows explicitly and in the open, not through a
-- cascade nobody sees fire.
ALTER TABLE translation_history
    DROP CONSTRAINT translation_history_key_id_fkey,
    ADD CONSTRAINT translation_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE;

ALTER TABLE key_history
    DROP CONSTRAINT key_history_key_id_fkey,
    ADD CONSTRAINT key_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id) ON DELETE CASCADE;
