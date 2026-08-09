package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCrossProjectPairingIsRefused proves the composite foreign keys do the
// work the design assigns them. These INSERTs are the corruption the whole
// scoping scheme exists to prevent, so the test asserts they FAIL — a test
// that only proved valid rows insert would pass just as happily against a
// schema with no constraints at all.
func TestCrossProjectPairingIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('scopetest', 'Scope Test')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
	})

	// A locale belonging to the other project.
	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))

	// A key belonging to YouTrip. sort_index has no default (see V1.00), so it
	// must be supplied even though this test has nothing to do with ordering.
	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'scope.test.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, _ = testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
	})

	t.Run("translation pairing across projects", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version)
			 VALUES (1, $1, $2, 'x', 1)`, youtripKey, otherLocale)
		requireRejected(t, err, "YouTrip key with another project's locale")
	})

	t.Run("translation claiming the wrong project", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version)
			 VALUES ($1, $2, $3, 'x', 1)`, otherProject, youtripKey, otherLocale)
		requireRejected(t, err, "another project claiming a YouTrip key")
	})
}

// TestLocaleExportDirectoriesAreUniquePerProject: two locales aiming at one
// directory would make the export zip silently overwrite one with the other.
func TestLocaleExportDirectoriesAreUniquePerProject(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'xx-XX', 'en_SG', 'values-xx', 'xx.lproj', 99)`)
	requireRejected(t, err, "duplicate flutter_dir within a project")

	_, err = testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'yy-YY', 'yy_YY', 'values', 'yy.lproj', 99)`)
	requireRejected(t, err, "duplicate android_values_dir within a project")
}
