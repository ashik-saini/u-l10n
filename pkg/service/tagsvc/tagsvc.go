// Package tagsvc owns workflow tags: the labels — "needs-review",
// "legal-approved", "q3-campaign" — that drive who looks at what.
//
// Every mutation here writes an audit_events row, and that is the reason this
// package exists rather than the handlers calling TagRepository directly.
//
// key_tags has NO history table. translations and keys both have one, so a value
// or a rename can always be traced; a tag assignment cannot. Worse, tags.id
// cascades into key_tags, so deleting one tag silently detaches it from every
// key that carried it, with no undo and no record that it was ever there.
// "legal-approved" is a claim about customer-facing copy, and a claim nobody can
// attribute is a claim nobody can rely on — so the audit row is not decoration,
// it is the only trace these operations leave.
package tagsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

const (
	// maxTagNameLen bounds a tag name. Nothing in the schema caps it; a tag is
	// a chip in a filter bar and a 4KB one is not a tag.
	maxTagNameLen = 64

	// maxBulkKeys bounds one bulk assign or unassign. Comfortably above the
	// whole corpus, so "tag everything" works, while still refusing an array
	// large enough to be an attempt to make the process allocate.
	maxBulkKeys = 20000

	// auditKeyIDLimit is how many key ids a delete records individually.
	//
	// Above it the audit row carries the count and a truncated flag. The ids are
	// recorded at all because the cascade is irreversible: with them, somebody
	// can put the tag back by hand.
	auditKeyIDLimit = 1000
)

// ErrBadRequest marks a caller error so the handler maps it to 400.
var ErrBadRequest = errors.New("bad request")

// Service performs tag writes. It owns every transaction boundary below, and
// every one of them contains the audit row as well as the change.
type Service struct {
	tx    database.Transactional
	tags  repository.TagRepository
	audit repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	tags repository.TagRepository,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, tags: tags, audit: audit}
}

// List returns every tag with the number of keys carrying it.
func (s *Service) List(ctx context.Context) ([]repository.TagUsage, error) {
	return s.tags.List(ctx, nil)
}

// Create adds a tag.
func (s *Service) Create(
	ctx context.Context, name, colour, actor, requestID string,
) (repository.Tag, error) {
	var created repository.Tag

	name, err := validateName(name)
	if err != nil {
		return created, err
	}
	colour, err = validateColour(colour)
	if err != nil {
		return created, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		t, err := s.tags.Create(ctx, tx, name, colour)
		if err != nil {
			return err
		}
		created = t

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionTagCreate,
			Target:    fmt.Sprintf("tag:%d", t.ID),
			Metadata:  map[string]any{"name": t.Name, "colour": t.Colour},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.Tag{}, err
	}

	log.Infow(ctx, "tag created", "tag_id", created.ID, "name", created.Name, "actor", actor)
	return created, nil
}

// Update renames or recolours a tag.
//
// The audit row records the name BEFORE and after. A rename is the one tag
// change that silently rewrites history everywhere the tag appears — every key
// carrying "needs-review" starts carrying "reviewed" without anything else
// changing — so the previous name has to be recoverable.
func (s *Service) Update(
	ctx context.Context, id int16, name, colour, actor, requestID string,
) (repository.Tag, error) {
	var updated repository.Tag

	name, err := validateName(name)
	if err != nil {
		return updated, err
	}
	colour, err = validateColour(colour)
	if err != nil {
		return updated, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// Read the previous state inside the transaction that changes it, so the
		// audit row records what actually preceded this write.
		before, err := s.tags.ByID(ctx, tx, id)
		if err != nil {
			return err
		}

		t, err := s.tags.Update(ctx, tx, id, name, colour)
		if err != nil {
			return err
		}
		updated = t

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionTagUpdate,
			Target: fmt.Sprintf("tag:%d", id),
			Metadata: map[string]any{
				"from_name": before.Name, "to_name": t.Name,
				"from_colour": before.Colour, "to_colour": t.Colour,
			},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.Tag{}, err
	}

	log.Infow(ctx, "tag updated", "tag_id", id, "name", updated.Name, "actor", actor)
	return updated, nil
}

// Delete removes a tag and, by cascade, every key's link to it.
//
// DESTRUCTIVE AND IRREVERSIBLE. key_tags.tag_id is ON DELETE CASCADE and
// key_tags has no history, so the fact that any key ever carried this tag
// disappears with the row. The audit metadata therefore records the affected key
// ids — read inside the same transaction, before the delete — so somebody can
// reconstruct the assignment by hand. Above auditKeyIDLimit ids it records the
// count and a truncated flag rather than an unbounded JSON document.
func (s *Service) Delete(ctx context.Context, id int16, actor, requestID string) (int, error) {
	var detached int

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		tag, err := s.tags.ByID(ctx, tx, id)
		if err != nil {
			return err
		}

		keyIDs, err := s.tags.KeyIDsWithTag(ctx, tx, id)
		if err != nil {
			return err
		}
		detached = len(keyIDs)

		metadata := map[string]any{
			"name":          tag.Name,
			"colour":        tag.Colour,
			"detached_keys": detached,
		}
		if detached <= auditKeyIDLimit {
			metadata["key_ids"] = keyIDs
		} else {
			metadata["key_ids_truncated"] = true
		}

		// The audit row is written BEFORE the delete, inside the same
		// transaction. Its failure fails the whole operation — the same choice
		// assetsvc makes for a presigned view, and for the same reason: a
		// destructive act with no record of what it destroyed is worse than the
		// act not happening.
		if err := s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionTagDelete,
			Target:    fmt.Sprintf("tag:%d", id),
			Metadata:  metadata,
			RequestID: requestID,
		}); err != nil {
			return err
		}

		return s.tags.Delete(ctx, tx, id)
	})
	if err != nil {
		return 0, err
	}

	log.Infow(ctx, "tag deleted", "tag_id", id, "detached_keys", detached, "actor", actor)
	return detached, nil
}

// SetKeyTags replaces a key's complete tag set.
//
// The caller sends what the key should END UP with, not a diff. An empty list
// clears every tag, and that is the documented way to say "no tags" rather than
// a no-op — otherwise there would be no way to remove the last one.
func (s *Service) SetKeyTags(
	ctx context.Context, keyID int64, tagIDs []int16, actor, requestID string,
) ([]repository.Tag, error) {
	var applied []repository.Tag

	if keyID <= 0 {
		return nil, fmt.Errorf("%w: key id must be positive", ErrBadRequest)
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// Every tag is resolved before anything is written, so a typo in one id
		// does not leave the key with a partially applied set.
		resolved := make([]repository.Tag, 0, len(tagIDs))
		ids := make([]int16, 0, len(tagIDs))
		for _, id := range tagIDs {
			tag, err := s.tags.ByID(ctx, tx, id)
			if err != nil {
				return err
			}
			resolved = append(resolved, tag)
			ids = append(ids, tag.ID)
		}

		if err := s.tags.SetKeyTags(ctx, tx, keyID, ids); err != nil {
			return err
		}
		applied = resolved

		names := make([]string, len(resolved))
		for i, t := range resolved {
			names[i] = t.Name
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionTagSetOnKey,
			Target: fmt.Sprintf("key:%d", keyID),
			// The resulting set, not the diff — the request was a replacement,
			// and recording a diff would describe an operation nobody performed.
			Metadata:  map[string]any{"tags": names},
			RequestID: requestID,
		})
	})
	if err != nil {
		return nil, err
	}

	log.Infow(ctx, "key tags replaced", "key_id", keyID, "count", len(applied), "actor", actor)
	return applied, nil
}

// AddToKeys attaches one tag to many keys. Idempotent: keys already carrying it
// keep their original created_at rather than looking newly tagged.
func (s *Service) AddToKeys(
	ctx context.Context, tagID int16, keyIDs []int64, actor, requestID string,
) (BulkResult, error) {
	return s.bulk(ctx, tagID, keyIDs, true, actor, requestID)
}

// RemoveFromKeys detaches one tag from many keys.
func (s *Service) RemoveFromKeys(
	ctx context.Context, tagID int16, keyIDs []int64, actor, requestID string,
) (BulkResult, error) {
	return s.bulk(ctx, tagID, keyIDs, false, actor, requestID)
}

// BulkResult reports what a bulk assign or unassign actually did.
//
// Removed is carried separately from Requested because "detached 40 of the 50
// you asked for" is a different fact from "detached all 50" — the caller
// selected a page of keys, not all of which carried the tag — and the portal
// reports it. An assign has no equivalent number: ON CONFLICT DO NOTHING makes
// a re-run a no-op, and reporting "0 added" for a retried request would read as
// a failure.
type BulkResult struct {
	Tag repository.Tag
	// Requested is how many keys the caller named.
	Requested int
	// Removed is meaningful only for an unassign.
	Removed int64
}

func (s *Service) bulk(
	ctx context.Context, tagID int16, keyIDs []int64, add bool, actor, requestID string,
) (BulkResult, error) {
	result := BulkResult{Requested: len(keyIDs)}

	if len(keyIDs) == 0 {
		return result, fmt.Errorf("%w: at least one key id is required", ErrBadRequest)
	}
	if len(keyIDs) > maxBulkKeys {
		return result, fmt.Errorf("%w: at most %d keys per request, got %d",
			ErrBadRequest, maxBulkKeys, len(keyIDs))
	}
	for _, id := range keyIDs {
		if id <= 0 {
			return result, fmt.Errorf("%w: key ids must be positive, got %d", ErrBadRequest, id)
		}
	}

	action := repository.ActionTagUnassign
	if add {
		action = repository.ActionTagAssign
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		t, err := s.tags.ByID(ctx, tx, tagID)
		if err != nil {
			return err
		}
		result.Tag = t

		metadata := map[string]any{"tag": t.Name, "requested": len(keyIDs)}

		if add {
			if err := s.tags.AddToKeys(ctx, tx, keyIDs, tagID); err != nil {
				return err
			}
		} else {
			removed, err := s.tags.RemoveFromKeys(ctx, tx, keyIDs, tagID)
			if err != nil {
				return err
			}
			// Fewer than requested is normal, not an error: the caller selected
			// a page of keys and not all of them carried the tag.
			metadata["removed"] = removed
			result.Removed = removed
		}

		if len(keyIDs) <= auditKeyIDLimit {
			metadata["key_ids"] = keyIDs
		} else {
			metadata["key_ids_truncated"] = true
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    action,
			Target:    fmt.Sprintf("tag:%d", tagID),
			Metadata:  metadata,
			RequestID: requestID,
		})
	})
	if err != nil {
		return BulkResult{}, err
	}

	log.Infow(ctx, "tag bulk operation", "tag_id", tagID,
		"action", action, "keys", len(keyIDs), "actor", actor)
	return result, nil
}

// validateName enforces what the schema does not.
//
// tags.name has only a uniqueness constraint. A name with surrounding
// whitespace produces a tag that looks identical to another one in every UI and
// collides with nothing — the worst kind of duplicate, because it is invisible.
func validateName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name is required", ErrBadRequest)
	}
	if trimmed != name {
		return "", fmt.Errorf("%w: name must not begin or end with whitespace", ErrBadRequest)
	}
	if len(name) > maxTagNameLen {
		return "", fmt.Errorf("%w: name must be at most %d characters, got %d",
			ErrBadRequest, maxTagNameLen, len(name))
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return "", fmt.Errorf("%w: name must not contain control characters", ErrBadRequest)
	}
	return name, nil
}

// validateColour accepts an empty string or #rrggbb.
//
// The column defaults to ” and is never NULL, so empty means "no colour
// chosen" — a real state, since Lokalise does not expose tag colours through its
// API and the imported ones were transcribed by hand. Anything else must be a
// value a browser can actually render, or the chip disappears.
func validateColour(colour string) (string, error) {
	colour = strings.TrimSpace(colour)
	if colour == "" {
		return "", nil
	}
	if len(colour) != 7 || colour[0] != '#' {
		return "", fmt.Errorf("%w: colour must be empty or #rrggbb, got %q", ErrBadRequest, colour)
	}
	for _, r := range colour[1:] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return "", fmt.Errorf("%w: colour must be empty or #rrggbb, got %q", ErrBadRequest, colour)
		}
	}
	return strings.ToLower(colour), nil
}
