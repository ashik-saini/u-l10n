-- History foreign keys stop cascading: an audit row must survive its subject.
--
-- WHY. V1.01 created translation_history.key_id and key_history.key_id with
-- ON DELETE CASCADE, and the very same file argues the opposite position for
-- branch_id: "an audit record must survive whatever happens to the thing it
-- describes". The cascade contradicts that. These tables answer "who changed
-- the customer-facing text that caused the complaint", and a delete of the key
-- would take the answer with it.
--
-- WHEN WOULD IT EVEN FIRE. No production path hard-deletes a keys row —
-- DELETE /api/v1/keys/{id} is a soft delete to status = 'deleted', and the
-- merge deletes translations, never keys. The only way to reach these cascades
-- is a hand-typed DELETE in psql — which is precisely the moment the audit
-- trail matters most, and precisely the moment it would silently vanish.
--
-- THE REPLACEMENT. Plain NO ACTION foreign keys. A hard delete of a key that
-- still has history is now REFUSED rather than quietly amplified: whoever is
-- holding the psql prompt is told the audit trail exists and must decide, in
-- the open, what to do about it. The reference itself stays — history rows
-- must still name a real key at insert time; only the deletion behaviour
-- changes. translations keeps its cascade: a value is content, not audit, and
-- removing content with its key is the correct amplification.
ALTER TABLE translation_history
    DROP CONSTRAINT translation_history_key_id_fkey,
    ADD CONSTRAINT translation_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id);

ALTER TABLE key_history
    DROP CONSTRAINT key_history_key_id_fkey,
    ADD CONSTRAINT key_history_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES keys (id);
