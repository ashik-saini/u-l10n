package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// AuditEvent records an action, as opposed to a value change — key and
// translation history already cover the latter.
type AuditEvent struct {
	Actor  string
	Action string
	// Target is a free-form reference such as "asset:88" or "key:1234".
	// Deliberately not a foreign key: an audit record must outlive whatever it
	// describes.
	Target    string
	Metadata  map[string]any
	RequestID string
}

// Audit action names. Constants rather than string literals at the call sites,
// because these are queried by name and a typo produces a row that no
// investigation will ever find.
const (
	ActionAssetCreate = "asset.create"
	// ActionAssetView is written whenever a presigned GET is issued. These are
	// screenshots of a fintech app and routinely contain customer names,
	// balances and transaction history, so "who looked at this" must be
	// answerable.
	ActionAssetView   = "asset.view"
	ActionAssetAttach = "asset.attach"
	ActionAssetDetach = "asset.detach"
)

// AuditRepository appends to the audit trail.
//
// A repository rather than an inline Exec in each service, so the JSON encoding
// and the column list exist once. It takes the usual optional tx: an audit row
// written outside the transaction it describes can survive a rollback and
// testify to something that never happened.
type AuditRepository interface {
	Record(ctx context.Context, tx *gorm.DB, e AuditEvent) error
}

type auditRepository struct{ base }

func ProvideAuditRepository(connector database.GORMConnector) AuditRepository {
	return &auditRepository{base{connector: connector}}
}

func (r *auditRepository) Record(ctx context.Context, tx *gorm.DB, e AuditEvent) error {
	metadata := []byte(`{}`)
	if len(e.Metadata) > 0 {
		encoded, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("encode audit metadata for %s: %w", e.Action, err)
		}
		metadata = encoded
	}

	err := r.db(ctx, tx).Exec(`
		INSERT INTO audit_events (actor, action, target, metadata, request_id)
		VALUES ($1, $2, $3, $4, $5)`,
		e.Actor, e.Action, e.Target, string(metadata), e.RequestID).Error
	if err != nil {
		return fmt.Errorf("record audit event %s: %w", e.Action, err)
	}
	return nil
}
