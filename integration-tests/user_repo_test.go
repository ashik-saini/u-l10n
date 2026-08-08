package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These mirror the statements pkg/repository/user.go issues, following the same
// convention as asset_repo_test.go: the behaviour under test IS the SQL, and a
// mock would only prove the mock agrees with itself.
//
// Two things here are never checked by a compiler and are load-bearing for
// authorization: the ON CONFLICT clause in upsertUserSQL, and the CITEXT
// column's case-insensitivity — which is the only reason the repository does
// not lowercase addresses at every call site.

// upsertUser mirrors upsertUserSQL.
func upsertUser(t *testing.T, email, role, status string) (id int64, gotRole, gotStatus string) {
	t.Helper()
	err := testDB.QueryRow(`
		INSERT INTO users (email, role, status)
		VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE
		   SET role = EXCLUDED.role, status = EXCLUDED.status, updated_at = now()
		RETURNING id, role, status`,
		email, role, status).Scan(&id, &gotRole, &gotStatus)
	require.NoError(t, err)
	return id, gotRole, gotStatus
}

// TestUserUpsertIsIdempotentAndPromotes. `user grant` is the bootstrap path and
// is expected to be re-run; a second run must promote rather than fail on a
// unique violation.
func TestUserUpsertIsIdempotentAndPromotes(t *testing.T) {
	first, role, status := upsertUser(t, "upsert@you.co", "viewer", "active")
	assert.Equal(t, "viewer", role)
	assert.Equal(t, "active", status)

	second, role, status := upsertUser(t, "upsert@you.co", "admin", "active")
	assert.Equal(t, first, second, "the same person, not a second row")
	assert.Equal(t, "admin", role)
	assert.Equal(t, "active", status)

	var count int
	require.NoError(t, testDB.QueryRow(
		`SELECT count(*) FROM users WHERE email = $1`, "upsert@you.co").Scan(&count))
	assert.Equal(t, 1, count)
}

// TestUserEmailIsCaseInsensitive proves the CITEXT column carries the rule.
//
// If this ever fails, every ByEmail lookup in the middleware becomes
// case-sensitive and an operator whose Google profile capitalises their name
// silently stops being provisioned — a 403 that no amount of re-authentication
// fixes.
func TestUserEmailIsCaseInsensitive(t *testing.T) {
	first, _, _ := upsertUser(t, "Case.Test@You.co", "editor", "active")

	// A different casing is the same row, both to the unique constraint...
	second, role, _ := upsertUser(t, "case.test@you.co", "approver", "active")
	assert.Equal(t, first, second)
	assert.Equal(t, "approver", role)

	// ...and to the lookup the middleware performs on every request.
	var got string
	require.NoError(t, testDB.QueryRow(
		`SELECT role FROM users WHERE email = $1`, "CASE.TEST@YOU.CO").Scan(&got))
	assert.Equal(t, "approver", got)
}

// TestUserSetRoleReportsAMissingUser mirrors setRoleSQL. RETURNING yields no
// row when nothing matched, which is how the repository tells "changed it" from
// "there was nobody to change" — a typo'd address must be a 404, not a silent
// success.
func TestUserSetRoleReportsAMissingUser(t *testing.T) {
	upsertUser(t, "setrole@you.co", "viewer", "active")

	var role string
	err := testDB.QueryRow(
		`UPDATE users SET role = $2, updated_at = now() WHERE email = $1 RETURNING role`,
		"setrole@you.co", "editor").Scan(&role)
	require.NoError(t, err)
	assert.Equal(t, "editor", role)

	err = testDB.QueryRow(
		`UPDATE users SET role = $2, updated_at = now() WHERE email = $1 RETURNING role`,
		"nobody-here@you.co", "admin").Scan(&role)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rows")
}

// TestUserUpdatedAtMoves. There is no trigger on this table, so the statement
// has to set the column itself; a column that only ever holds its insert-time
// default is worse than no column at all.
func TestUserUpdatedAtMoves(t *testing.T) {
	upsertUser(t, "touched@you.co", "viewer", "active")

	var before, after string
	require.NoError(t, testDB.QueryRow(
		`SELECT updated_at::text FROM users WHERE email = $1`, "touched@you.co").Scan(&before))

	upsertUser(t, "touched@you.co", "editor", "active")

	require.NoError(t, testDB.QueryRow(
		`SELECT updated_at::text FROM users WHERE email = $1`, "touched@you.co").Scan(&after))
	assert.NotEqual(t, before, after)
}

// TestUserRoleAndStatusAreConstrained. The service validates first so a typo is
// a 400 rather than a 500, but the database is the last line of defence and a
// constraint nobody has watched reject anything is a constraint nobody can
// trust.
func TestUserRoleAndStatusAreConstrained(t *testing.T) {
	_, err := testDB.Exec(
		`INSERT INTO users (email, role) VALUES ($1, $2)`, "bad-role@you.co", "superadmin")
	requireRejected(t, err, "a role outside the ordered set")

	_, err = testDB.Exec(
		`INSERT INTO users (email, status) VALUES ($1, $2)`, "bad-status@you.co", "suspended")
	requireRejected(t, err, "an unknown status")
}
