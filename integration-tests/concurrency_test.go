package integrationtests

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The concurrency properties of the merge transaction, demonstrated rather than
// argued.
//
// Everything here needs genuinely concurrent transactions, so it cannot be
// unit-tested: a mock would only prove the mock serialises. These are the tests
// that turn "the advisory lock should work" into "the advisory lock does work".

const mergeLockKey int64 = 8_675_309

// TestAdvisoryLockSerialisesMerges is the important one.
//
// Two merges run concurrently. Postgres defaults to READ COMMITTED, which does
// NOT stop both from reading a consistent-looking world and both committing —
// so without the lock, the second merge would compute its conflicts against a
// master that the first is about to change, and would then overwrite it.
//
// The assertion is on OVERLAP: the second transaction must not enter its
// critical section until the first has left it.
func TestAdvisoryLockSerialisesMerges(t *testing.T) {
	type window struct{ enter, exit time.Time }
	windows := make([]window, 2)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximise the chance of a genuine race

			tx, err := testDB.Begin()
			require.NoError(t, err)
			defer tx.Rollback() //nolint:errcheck

			// Same key both sides. A different integer here would mean each
			// caller believed it held "the" lock while the other ran.
			_, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, mergeLockKey)
			require.NoError(t, err)

			windows[i].enter = time.Now()
			// Hold the critical section long enough that an overlap would be
			// unambiguous rather than a scheduling artefact.
			time.Sleep(150 * time.Millisecond)
			windows[i].exit = time.Now()

			require.NoError(t, tx.Commit())
		}(i)
	}

	close(start)
	wg.Wait()

	// One window must end before the other begins, in whichever order the
	// scheduler picked.
	first, second := windows[0], windows[1]
	if second.enter.Before(first.enter) {
		first, second = second, first
	}
	assert.False(t, second.enter.Before(first.exit),
		"critical sections overlapped: second entered at %s, first exited at %s — the advisory lock is not serialising merges",
		second.enter.Format(time.StampMilli), first.exit.Format(time.StampMilli))
}

// TestLockOrderingPreventsDeadlock demonstrates why the merge locks rows
// ORDER BY key_id, locale_id.
//
// Two transactions locking an overlapping row set in OPPOSITE orders deadlock:
// A holds row 1 and wants row 2 while B holds row 2 and wants row 1. Postgres
// detects it and kills one with SQLSTATE 40P01 — an ugly 500 for a user who did
// nothing wrong.
//
// u-reward/pkg/repository/reward_repo.go:368,375 locks by id IN (...) with no
// ORDER BY, which is exactly this shape. The merge deliberately does not copy it.
func TestLockOrderingPreventsDeadlock(t *testing.T) {
	enSG := localeID(t, "en-SG")
	keyA := insertKey(t, "deadlock_row_a")
	keyB := insertKey(t, "deadlock_row_b")

	for _, k := range []int64{keyA, keyB} {
		_, err := testDB.Exec(`
			INSERT INTO translations (key_id, locale_id, value, updated_by)
			VALUES ($1, $2, 'v', 'test@you.co')`, k, enSG)
		require.NoError(t, err)
	}

	// lockBoth acquires both rows in ASCENDING key order — what the merge does.
	lockBoth := func(ctx context.Context) error {
		tx, err := testDB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck

		if _, err := tx.ExecContext(ctx, `
			SELECT key_id FROM translations
			 WHERE key_id = ANY($1) AND locale_id = $2
			 ORDER BY key_id, locale_id
			   FOR UPDATE`, int64Array(keyA, keyB), enSG); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
		return tx.Commit()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = lockBoth(ctx) }(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err,
			"transaction %d failed; a deadlock here would mean the ordering guarantee is not holding", i)
	}
}

// TestConcurrentOCCUpdatesLoseExactlyOne proves optimistic concurrency control
// under real contention: several writers read the same version and race to
// update. Exactly one may win; the rest must affect zero rows, which is the
// signal the handler turns into a 409.
//
// The failure this rules out is the silent one — two writers both "succeeding"
// and the second quietly discarding the first's words.
func TestConcurrentOCCUpdatesLoseExactlyOne(t *testing.T) {
	keyID := insertKey(t, "concurrent_occ_case")
	enSG := localeID(t, "en-SG")

	_, err := testDB.Exec(`
		INSERT INTO translations (key_id, locale_id, value, updated_by)
		VALUES ($1, $2, 'original', 'a@you.co')`, keyID, enSG)
	require.NoError(t, err)

	const writers = 8
	var wg sync.WaitGroup
	won := make([]bool, writers)
	start := make(chan struct{})

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start

			// Every writer read version 1.
			res, err := testDB.Exec(`
				UPDATE translations
				   SET value = $1, version = version + 1, updated_by = $2
				 WHERE key_id = $3 AND locale_id = $4 AND version = 1`,
				"writer", "w@you.co", keyID, enSG)
			require.NoError(t, err)

			n, err := res.RowsAffected()
			require.NoError(t, err)
			won[i] = n == 1
		}(i)
	}

	close(start)
	wg.Wait()

	var winners int
	for _, w := range won {
		if w {
			winners++
		}
	}
	assert.Equal(t, 1, winners,
		"exactly one writer may win a version race; %d winners means lost updates are possible", winners)

	var version int
	require.NoError(t, testDB.QueryRow(
		`SELECT version FROM translations WHERE key_id = $1 AND locale_id = $2`,
		keyID, enSG).Scan(&version))
	assert.Equal(t, 2, version, "the version must advance exactly once")
}

// int64Array builds a Postgres bigint[] literal without pulling lib/pq's array
// helper into the test package.
func int64Array(vs ...int64) interface{} {
	out := "{"
	for i, v := range vs {
		if i > 0 {
			out += ","
		}
		out += itoa(v)
	}
	return out + "}"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

var _ = sql.ErrNoRows
