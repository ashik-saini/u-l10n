package usersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// fakeUsers is a UserRepository backed by a map, keyed case-insensitively
// because the real column is CITEXT.
type fakeUsers struct {
	rows      map[string]repository.User
	setRoleAt int // how many times SetRole was called
	upsertAt  int
	err       error
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{rows: map[string]repository.User{}}
}

func key(email string) string {
	out := make([]byte, len(email))
	for i := 0; i < len(email); i++ {
		c := email[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func (f *fakeUsers) ByEmail(_ context.Context, _ *gorm.DB, email string) (repository.User, error) {
	if f.err != nil {
		return repository.User{}, f.err
	}
	u, ok := f.rows[key(email)]
	if !ok {
		return repository.User{}, repository.ErrNotFound
	}
	return u, nil
}

func (f *fakeUsers) Upsert(_ context.Context, _ *gorm.DB, u repository.User) (repository.User, error) {
	f.upsertAt++
	if f.err != nil {
		return repository.User{}, f.err
	}
	f.rows[key(u.Email)] = u
	return u, nil
}

func (f *fakeUsers) List(context.Context, *gorm.DB) ([]repository.User, error) {
	out := make([]repository.User, 0, len(f.rows))
	for _, u := range f.rows {
		out = append(out, u)
	}
	return out, f.err
}

func (f *fakeUsers) SetRole(_ context.Context, _ *gorm.DB, email, role string) (repository.User, error) {
	f.setRoleAt++
	if f.err != nil {
		return repository.User{}, f.err
	}
	u, ok := f.rows[key(email)]
	if !ok {
		return repository.User{}, repository.ErrNotFound
	}
	u.Role = role
	f.rows[key(email)] = u
	return u, nil
}

type fakeAudit struct {
	events []repository.AuditEvent
	err    error
}

func (f *fakeAudit) Record(_ context.Context, _ *gorm.DB, e repository.AuditEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}

// fakeTx runs the body directly. The real helper does not nest, and neither
// does this: exactly one transaction per service call.
type fakeTx struct{ calls int }

func (f *fakeTx) WithTransaction(_ context.Context, fn database.TransactionFunc) error {
	f.calls++
	return fn(nil)
}

type harness struct {
	svc   *Service
	users *fakeUsers
	audit *fakeAudit
	tx    *fakeTx
}

func newHarness() *harness {
	h := &harness{users: newFakeUsers(), audit: &fakeAudit{}, tx: &fakeTx{}}
	h.svc = &Service{tx: h.tx, users: h.users, audit: h.audit}
	return h
}

// --- SetRole ----------------------------------------------------------------

func TestSetRoleChangesTheRoleAndAuditsIt(t *testing.T) {
	h := newHarness()
	h.users.rows["editor@you.co"] = repository.User{
		Email: "editor@you.co", Role: repository.RoleEditor, Status: repository.StatusActive}

	updated, err := h.svc.SetRole(context.Background(),
		"editor@you.co", repository.RoleApprover, "admin@you.co", "req-1")
	require.NoError(t, err)
	assert.Equal(t, repository.RoleApprover, updated.Role)

	require.Len(t, h.audit.events, 1)
	e := h.audit.events[0]
	assert.Equal(t, repository.ActionUserRoleChange, e.Action)
	assert.Equal(t, "admin@you.co", e.Actor)
	assert.Equal(t, "user:editor@you.co", e.Target)
	assert.Equal(t, "req-1", e.RequestID)
	// Both ends of the change, so the trail answers "what did they have before"
	// without replaying every earlier row.
	assert.Equal(t, repository.RoleEditor, e.Metadata["from"])
	assert.Equal(t, repository.RoleApprover, e.Metadata["to"])

	assert.Equal(t, 1, h.tx.calls, "one transaction, so the row and its audit land together")
}

// TestSetRoleRefusesAnUnknownRole. The users_role_check constraint would refuse
// it too, but as a 500 with a Postgres error string in it. A typo must read as
// a typo.
func TestSetRoleRefusesAnUnknownRole(t *testing.T) {
	for _, role := range []string{"", "superadmin", "Admin", "ADMIN", "admin "} {
		t.Run(role, func(t *testing.T) {
			h := newHarness()
			h.users.rows["a@you.co"] = repository.User{
				Email: "a@you.co", Role: repository.RoleViewer, Status: repository.StatusActive}

			_, err := h.svc.SetRole(context.Background(), "a@you.co", role, "admin@you.co", "req")
			assert.ErrorIs(t, err, ErrBadRequest)
			assert.Zero(t, h.users.setRoleAt, "nothing may be written on a refused request")
			assert.Empty(t, h.audit.events)
		})
	}
}

// TestSetRoleDoesNotCreateUsers: a typo'd address is a 404, not a new account
// for a person who does not exist.
func TestSetRoleDoesNotCreateUsers(t *testing.T) {
	h := newHarness()

	_, err := h.svc.SetRole(context.Background(),
		"typo@you.co", repository.RoleAdmin, "admin@you.co", "req")
	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.Empty(t, h.users.rows)
	assert.Empty(t, h.audit.events)
}

func TestSetRoleRefusesRubbishEmails(t *testing.T) {
	for _, email := range []string{"", "   ", "not-an-email", "@you.co", "a@", "a b@you.co"} {
		t.Run(email, func(t *testing.T) {
			h := newHarness()
			_, err := h.svc.SetRole(context.Background(),
				email, repository.RoleViewer, "admin@you.co", "req")
			assert.ErrorIs(t, err, ErrBadRequest)
		})
	}
}

// TestSetRoleFailsWhenTheAuditFails.
//
// The audit row and the privilege change share a transaction, so losing the
// record means losing the change. A privilege grant nobody can account for is
// worse than a failed request.
func TestSetRoleFailsWhenTheAuditFails(t *testing.T) {
	h := newHarness()
	h.audit.err = errors.New("audit table is full")
	h.users.rows["a@you.co"] = repository.User{
		Email: "a@you.co", Role: repository.RoleViewer, Status: repository.StatusActive}

	_, err := h.svc.SetRole(context.Background(),
		"a@you.co", repository.RoleAdmin, "admin@you.co", "req")
	require.Error(t, err)
	assert.Equal(t, 1, h.tx.calls,
		"the error must propagate out of the transaction so it rolls back")
}

// --- Grant ------------------------------------------------------------------

// TestGrantCreatesTheFirstAdmin is the bootstrap case: an empty users table has
// no admin, so no API request can ever mint one.
func TestGrantCreatesTheFirstAdmin(t *testing.T) {
	h := newHarness()

	granted, err := h.svc.Grant(context.Background(),
		"ashik.saini@you.co", repository.RoleAdmin, "", "ashik.saini@you.co", "cli")
	require.NoError(t, err)
	assert.Equal(t, repository.RoleAdmin, granted.Role)
	// Status defaults to active: an operator created without one is meant to be
	// able to sign in.
	assert.Equal(t, repository.StatusActive, granted.Status)

	require.Len(t, h.audit.events, 1)
	e := h.audit.events[0]
	assert.Equal(t, repository.ActionUserGrant, e.Action)
	assert.Equal(t, "(none)", e.Metadata["from"], "absent and present are different facts")
	assert.Equal(t, "cli", e.Metadata["via"])
}

func TestGrantIsIdempotentAndPromotes(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	_, err := h.svc.Grant(ctx, "a@you.co", repository.RoleViewer, "", "boot@you.co", "cli")
	require.NoError(t, err)

	promoted, err := h.svc.Grant(ctx, "a@you.co", repository.RoleAdmin, "", "boot@you.co", "cli")
	require.NoError(t, err)
	assert.Equal(t, repository.RoleAdmin, promoted.Role)
	assert.Len(t, h.users.rows, 1, "re-granting must not create a second row")

	require.Len(t, h.audit.events, 2)
	assert.Equal(t, repository.RoleViewer, h.audit.events[1].Metadata["from"])
}

func TestGrantRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown role", func(t *testing.T) {
		h := newHarness()
		_, err := h.svc.Grant(ctx, "a@you.co", "root", "", "boot@you.co", "cli")
		assert.ErrorIs(t, err, ErrBadRequest)
		assert.Zero(t, h.users.upsertAt)
	})

	t.Run("unknown status", func(t *testing.T) {
		h := newHarness()
		_, err := h.svc.Grant(ctx, "a@you.co", repository.RoleAdmin, "suspended", "boot@you.co", "cli")
		assert.ErrorIs(t, err, ErrBadRequest)
		assert.Zero(t, h.users.upsertAt)
	})

	t.Run("no actor", func(t *testing.T) {
		// audit_events.actor is NOT NULL, and a blank actor answers nothing. The
		// CLI has no authenticated principal, so the human must name themselves.
		h := newHarness()
		_, err := h.svc.Grant(ctx, "a@you.co", repository.RoleAdmin, "", "  ", "cli")
		assert.ErrorIs(t, err, ErrBadRequest)
		assert.Zero(t, h.users.upsertAt)
	})

	t.Run("rubbish email", func(t *testing.T) {
		h := newHarness()
		_, err := h.svc.Grant(ctx, "nobody", repository.RoleAdmin, "", "boot@you.co", "cli")
		assert.ErrorIs(t, err, ErrBadRequest)
		assert.Zero(t, h.users.upsertAt)
	})
}

// TestGrantCanDisable: disabling is how access is withdrawn, and it must work
// from the shell for the same reason granting does — the person being disabled
// may be the only admin.
func TestGrantCanDisable(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	_, err := h.svc.Grant(ctx, "gone@you.co", repository.RoleAdmin, repository.StatusDisabled, "boot@you.co", "cli")
	require.NoError(t, err)
	assert.Equal(t, repository.StatusDisabled, h.users.rows["gone@you.co"].Status)
}

// TestEmailIsNotLowercased. users.email is CITEXT, so case is handled by the
// column type; a second normalisation rule here would be one more place for the
// two to drift apart.
func TestEmailIsNotLowercased(t *testing.T) {
	got, err := normaliseEmail("  Ashik.Saini@You.co  ")
	require.NoError(t, err)
	assert.Equal(t, "Ashik.Saini@You.co", got)
}
