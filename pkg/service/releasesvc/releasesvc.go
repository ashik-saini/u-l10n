// Package releasesvc owns what ships: the release history, the manual publish,
// and the OTA kill switch.
//
// A release is an immutable numbered snapshot of master with one materialised
// bundle per locale. Every merge cuts one inside its own transaction, which is
// what makes the export and OTA endpoints read-only handlers over identical
// precomputed rows — they cannot disagree, because there is nothing left to
// disagree about.
//
// This package adds the two release paths that have no merge behind them, and
// both are operationally sharp:
//
//   - Publish ships master to every app WITHOUT review. It exists because
//     master can be correct while no merge request is open — an import, a
//     direct fix — and because a rollback needs something to roll forward to.
//   - Rollback is the kill switch. It removes a release from serving and clients
//     fall back to the strings compiled into the app binary.
package releasesvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

const (
	maxNotesLen = 4000

	// DefaultListLimit is one page of release history.
	DefaultListLimit = 50

	// maxListLimit bounds a page. Releases accumulate one per merge, so this is
	// generous; it exists to refuse a request whose response could not be built.
	maxListLimit = 500
)

var (
	// ErrBadRequest marks a caller error so the handler maps it to 400.
	ErrBadRequest = errors.New("bad request")

	// ErrAlreadyRolledBack re-exports the repository sentinel, so a caller of
	// this package matches one name rather than reaching through it.
	ErrAlreadyRolledBack = repository.ErrAlreadyRolledBack
)

// Service manages releases. It owns every transaction boundary below.
type Service struct {
	tx       database.Transactional
	releases repository.ReleaseRepository
	locales  repository.LocaleRepository
	rows     repository.ExportRowReader
	audit    repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	releases repository.ReleaseRepository,
	locales repository.LocaleRepository,
	rows repository.ExportRowReader,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, releases: releases, locales: locales, rows: rows, audit: audit}
}

// List returns release history, newest first.
func (s *Service) List(ctx context.Context, limit, offset int) ([]repository.ReleaseDetail, error) {
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 0 || limit > maxListLimit {
		return nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrBadRequest, maxListLimit)
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset must not be negative", ErrBadRequest)
	}
	return s.releases.List(ctx, nil, limit, offset)
}

// Get returns one release by its human-facing version number.
func (s *Service) Get(ctx context.Context, version int64) (repository.ReleaseDetail, error) {
	if version <= 0 {
		return repository.ReleaseDetail{},
			fmt.Errorf("%w: version must be a positive integer", ErrBadRequest)
	}
	return s.releases.ByVersion(ctx, nil, version)
}

// Bundle returns one locale's materialised strings for a release.
//
// This is the portal's "what exactly did release 41 ship to Thai?" view. It
// reads the stored bundle rather than re-deriving it from master: the whole
// point of materialising is that the answer cannot drift after the fact.
func (s *Service) Bundle(
	ctx context.Context, version int64, localeCode string,
) (repository.Bundle, error) {
	var bundle repository.Bundle

	if version <= 0 {
		return bundle, fmt.Errorf("%w: version must be a positive integer", ErrBadRequest)
	}

	// TODO(plan-2): the scope arrives from the request path once routes are
	// project-prefixed. Hardcoded to YouTrip until then.
	locale, err := s.locales.ByCode(ctx, nil, 1, localeCode)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// Locales are reference data seeded by migration, so an unknown code
			// is a caller mistake rather than a missing resource.
			return bundle, fmt.Errorf("%w: unknown locale %q", ErrBadRequest, localeCode)
		}
		return bundle, err
	}

	return s.releases.BundleFor(ctx, nil, version, locale.ID)
}

// Publish cuts a release from master's CURRENT state, with no merge behind it.
//
// The bundles are materialised in the SAME transaction as the release row, for
// the same reason the merge does it: a release whose bundles landed a moment
// later would be servable while empty, and an OTA client that caught that window
// would cache an empty bundle behind an ETag and stop asking.
//
// Approver-only at the route, because this is the one path that puts copy in
// front of customers without anybody reviewing a diff.
func (s *Service) Publish(
	ctx context.Context, notes, minAppVersion, actor, requestID string,
) (repository.ReleaseDetail, error) {
	var published repository.ReleaseDetail

	if len(notes) > maxNotesLen {
		return published, fmt.Errorf("%w: notes must be at most %d characters",
			ErrBadRequest, maxNotesLen)
	}
	floor, err := validateMinAppVersion(minAppVersion)
	if err != nil {
		return published, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		release, err := s.releases.CreatePublish(ctx, tx,
			strings.TrimSpace(notes), floor, actor)
		if err != nil {
			return err
		}

		// TODO(plan-2): the scope arrives from the request path once routes are
		// project-prefixed. Hardcoded to YouTrip until then.
		locales, err := s.locales.List(ctx, tx, 1, false)
		if err != nil {
			return err
		}
		for _, locale := range locales {
			// PlatformFlutter, matching the merge: the bundle is structurally
			// identical to assets/langs/<locale>.json in the mobile repo, so the
			// OTA payload and the asset compiled into the binary cannot drift.
			rows, err := s.rows.ForExport(ctx, tx, locale.ID, model.PlatformFlutter)
			if err != nil {
				return err
			}
			if err := s.releases.MaterialiseBundle(ctx, tx, release.ID, locale, rows); err != nil {
				return err
			}
		}

		if err := s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionReleasePublish,
			Target: fmt.Sprintf("release:%d", release.Version),
			Metadata: map[string]any{
				"locales":         len(locales),
				"min_app_version": minAppVersion,
				"notes":           notes,
			},
			RequestID: requestID,
		}); err != nil {
			return err
		}

		published, err = s.releases.ByVersion(ctx, tx, release.Version)
		return err
	})
	if err != nil {
		return repository.ReleaseDetail{}, err
	}

	log.Infow(ctx, "release published",
		"release", published.Version, "locales", published.LocaleCount, "actor", actor)
	return published, nil
}

// Rollback withholds a release from OTA serving. This is the kill switch.
//
// What it does NOT do is undo the values. master keeps whatever the merge
// applied; only the SERVED bundle changes, and clients fall back to the newest
// earlier release that is still eligible, or to the strings compiled into the
// binary. Undoing the data is a separate act — a branch with the old values and
// a merge — because a rollback happens in a hurry and must not also rewrite the
// corpus.
//
// Rolling back a release that is already rolled back is refused rather than
// repeated: rolled_back_by answers the only question anybody asks afterwards,
// and a second write would replace that name with whoever pressed it last.
func (s *Service) Rollback(
	ctx context.Context, version int64, actor, requestID string,
) (repository.ReleaseDetail, error) {
	var rolled repository.ReleaseDetail

	if version <= 0 {
		return rolled, fmt.Errorf("%w: version must be a positive integer", ErrBadRequest)
	}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		updated, err := s.releases.Rollback(ctx, tx, version, actor)
		if err != nil {
			return err
		}
		rolled = updated

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionReleaseRollback,
			Target: fmt.Sprintf("release:%d", version),
			Metadata: map[string]any{
				"source":           updated.Source,
				"merge_request_id": updated.MergeRequestID,
			},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.ReleaseDetail{}, err
	}

	// Deliberately louder than the rest of this package: somebody has just
	// withdrawn shipped copy from every client, and that belongs in the incident
	// timeline whether or not anybody queries audit_events.
	log.Infow(ctx, "RELEASE ROLLED BACK — the OTA kill switch was pulled",
		"release", version, "actor", actor)
	return rolled, nil
}

// validateMinAppVersion enforces exactly three numeric components.
//
// This is not cosmetic. The serving query compares floors as an INTEGER TRIPLE:
//
//	string_to_array(r.min_app_version, '.')::int[] <= string_to_array(client, '.')::int[]
//
// A value like "4.12-beta" makes that cast fail at READ time, on the
// unauthenticated OTA path, for every client asking for that locale — a bad
// publish would take string delivery down rather than merely being rejected.
// "4.12" is refused for a subtler reason: Postgres compares arrays element-wise
// and then by length, so ARRAY[4,12] < ARRAY[4,12,0], and a two-component floor
// would silently admit clients the publisher meant to exclude.
//
// An empty string means no floor at all, which is a real and common choice, so
// it returns nil rather than a pointer to "".
func validateMinAppVersion(raw string) (*string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf(
			"%w: min_app_version must be exactly three numeric components, like 4.12.0 — got %q",
			ErrBadRequest, raw)
	}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, fmt.Errorf(
				"%w: min_app_version must be exactly three numeric components, like 4.12.0 — got %q",
				ErrBadRequest, raw)
		}
	}

	return &raw, nil
}
