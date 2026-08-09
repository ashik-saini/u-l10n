// Package repository is the only place that speaks SQL.
//
// GORM v1 handles CRUD; anything needing a Postgres feature GORM v1 cannot
// express — ON CONFLICT, FOR UPDATE, advisory locks — drops to hand-written SQL
// through Exec or Raw. That split is the established house pattern, not an
// escape hatch: see u-reward/pkg/repository/campaign_entry.go.
package repository

import (
	"context"
	"errors"

	"github.com/google/wire"
	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"
)

var log = ulog.GetLogger("u-l10n")

// ErrOptimisticLock is returned when a version-guarded UPDATE affects zero
// rows, meaning another writer moved the row first.
//
// It is an exported sentinel so callers can match it with errors.Is and map it
// to HTTP 409. u-reward has two conventions for this — an exported sentinel in
// reward_repo.go and a bare errors.New("version conflict") in
// user_achievement.go that callers can only match by string. This follows the
// former.
var ErrOptimisticLock = errors.New("optimistic lock conflict: the row was modified by another writer")

// ErrNotFound is returned when a lookup finds nothing. Distinguished from an
// empty value on purpose: absent and blank are different facts.
var ErrNotFound = errors.New("not found")

var WireSet = wire.NewSet(
	ProvideProjectRepository,
	ProvideLocaleRepository,
	ProvideKeyRepository,
	ProvideTranslationRepository,
	ProvideExportRowReader,
	ProvideAPITokenRepository,
	ProvideBranchRepository,
	ProvideMergeRequestRepository,
	ProvideReleaseRepository,
	ProvideAssetRepository,
	ProvideAuditRepository,
	ProvideTagRepository,
	ProvideUserRepository,
	ProvideUserProjectRoleRepository,
)

// base carries the connection plumbing shared by every repository.
type base struct {
	connector database.GORMConnector
}

// db returns the handle to issue statements on.
//
// Every write method takes an optional tx. When non-nil the statement joins the
// caller's transaction; when nil it runs on the pooled connection. This is what
// lets a service compose several repositories into one transaction, and it is
// necessary because the shared Transactional.WithTransaction helper DOES NOT
// NEST — a repository opening its own transaction inside a caller's would
// silently split the work across two, and the atomicity the caller was relying
// on would evaporate without any error.
func (b *base) db(ctx context.Context, tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return b.connector.GetDBWithContext(ctx)
}
