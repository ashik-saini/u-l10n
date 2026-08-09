-- Undo for V1.09. Never executed; see U1.00.
--
-- Dropping this table drops the dimension every other table hangs from, so it
-- can only run after U1.10-U1.13 have removed their project_id columns.
DROP TABLE IF EXISTS projects;
