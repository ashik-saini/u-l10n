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
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// A locale belonging to the other project. Cleanup is registered here,
	// before the project's, so it runs FIRST (t.Cleanup is LIFO):
	// locales_project_fkey carries no ON DELETE CASCADE, so leaving this row
	// behind would make the project delete above fail — and with the error
	// discarded, that failure would be silent, leaking a project AND a second
	// 'en-SG' locale row on every run. TestLocalesSeed's unscoped
	// `count(*) FROM locales` and helpers_test.go's unscoped
	// `WHERE code = $1` lookup would then depend on file ordering to stay
	// green, going red only under -shuffle=on or a narrowed -run.
	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'en-SG', 'en_SG', 'values', 'en-SG.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
		require.NoError(t, cleanupErr)
	})

	// A key belonging to YouTrip. sort_index has no default (see V1.00), so it
	// must be supplied even though this test has nothing to do with ordering.
	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'scope.test.key', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, cleanupErr)
	})

	t.Run("translation pairing across projects", func(t *testing.T) {
		// updated_by is CITEXT NOT NULL with no default (V1.00). Omitting it
		// gets the INSERT rejected during tuple formation (23502, before the
		// row is checked against ANY foreign key), which would make this
		// subtest pass against a schema with zero cross-project protection.
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version, updated_by)
			 VALUES (1, $1, $2, 'x', 1, 'test@you.co')`, youtripKey, otherLocale)
		requireRejected(t, err, "translations_locale_fkey",
			"YouTrip key with another project's locale")
	})

	t.Run("translation claiming the wrong project", func(t *testing.T) {
		_, err := testDB.Exec(
			`INSERT INTO translations (project_id, key_id, locale_id, value, version, updated_by)
			 VALUES ($1, $2, $3, 'x', 1, 'test@you.co')`, otherProject, youtripKey, otherLocale)
		requireRejected(t, err, "translations_key_fkey",
			"another project claiming a YouTrip key")
	})
}

// TestKeyTagPairingIsRefused proves key_tags_key_fkey and key_tags_tag_fkey
// individually — the translations test above exercises the analogous pair on
// translations, but key_tags carries its own two composite foreign keys and,
// before this test, neither had any coverage at all.
func TestKeyTagPairingIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('scopetest2', 'Scope Test 2')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// A tag belonging to the other project. Cleanup registered before the
	// project's so it runs first, for the same reason as the locale above:
	// tags_project_fkey has no ON DELETE CASCADE.
	var otherTag int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO tags (project_id, name) VALUES ($1, 'scope-test-tag')
		 RETURNING id`, otherProject).Scan(&otherTag))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM tags WHERE id = $1`, otherTag)
		require.NoError(t, cleanupErr)
	})

	// A key belonging to YouTrip.
	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'scope.test.tagkey', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, cleanupErr)
	})

	t.Run("YouTrip key tagged with another project's tag", func(t *testing.T) {
		// (project_id, key_id) = (1, youtripKey) is a real row in keys, so
		// key_tags_key_fkey is satisfied. (project_id, tag_id) = (1, otherTag)
		// is not — otherTag belongs to otherProject — so only
		// key_tags_tag_fkey can be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO key_tags (project_id, key_id, tag_id) VALUES (1, $1, $2)`,
			youtripKey, otherTag)
		requireRejected(t, err, "key_tags_tag_fkey",
			"YouTrip key tagged with another project's tag")
	})

	t.Run("another project claiming a YouTrip key", func(t *testing.T) {
		// (project_id, tag_id) = (otherProject, otherTag) is a real row in
		// tags, so key_tags_tag_fkey is satisfied. (project_id, key_id) =
		// (otherProject, youtripKey) is not — youtripKey belongs to project 1
		// — so only key_tags_key_fkey can be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO key_tags (project_id, key_id, tag_id) VALUES ($1, $2, $3)`,
			otherProject, youtripKey, otherTag)
		requireRejected(t, err, "key_tags_key_fkey",
			"another project claiming a YouTrip key via key_tags")
	})
}

// TestLocaleExportDirectoriesAreUniquePerProject: two locales aiming at one
// directory would make the export zip silently overwrite one with the other.
func TestLocaleExportDirectoriesAreUniquePerProject(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'xx-XX', 'en_SG', 'values-xx', 'xx.lproj', 99)`)
	requireRejected(t, err, "locales_project_flutter_dir_unique",
		"duplicate flutter_dir within a project")

	_, err = testDB.Exec(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES (1, 'yy-YY', 'yy_YY', 'values', 'yy.lproj', 99)`)
	requireRejected(t, err, "locales_project_android_dir_unique",
		"duplicate android_values_dir within a project")
}
