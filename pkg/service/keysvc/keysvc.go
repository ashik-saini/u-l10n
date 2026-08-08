// Package keysvc owns the portal's read and write path over keys and their
// translations.
//
// Two things here are load-bearing and easy to break by simplification.
//
// THE THREE-STATE RULE. A (key, locale) pair is untranslated (no row),
// deliberately blank (a row holding "") or translated. Nothing in this package
// returns a bare string for a value: every read carries repository.Cell, whose
// Found field is the only thing keeping "no value" and "the empty string" apart.
// Collapsing them adds ~430 spurious keys to en-SG and deletes 3,664
// intentional blanks from ms-MY.
//
// THE BRANCH IS NOT MASTER. Every read takes the same copy-on-write path the
// merge does — one delta-or-master resolution rule, expressed once in SQL. A
// read that quietly returned master when the caller asked for a branch would
// show an editor their changes had not saved, and is the bug this package is
// most likely to grow.
package keysvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// Limits. Each mirrors something the database or the export already implies;
// rejecting here means a caller gets a sentence instead of a constraint
// violation surfacing as a 500.
const (
	// maxNameLen bounds a key name. Nothing in the schema caps it, but a name
	// is an identifier that ends up in generated Dart, Kotlin and Swift.
	maxNameLen = 255

	// maxValueLen bounds a translation. The longest string in the current
	// corpus is under 2KB; this leaves an order of magnitude of headroom while
	// still refusing a paste of an entire document.
	maxValueLen = 20000

	// maxDescriptionLen bounds the translator-facing note on a key.
	maxDescriptionLen = 2000

	// DefaultHistoryLimit is how far back a key's timeline reaches by default.
	DefaultHistoryLimit = 200
)

var (
	// ErrBadRequest marks a caller error, so the handler maps it to 400 rather
	// than 500. A misspelled locale is a typo, not a fault.
	ErrBadRequest = errors.New("bad request")

	// ErrBranchNotOpen means the branch exists but is merged or closed. Writing
	// to a merged branch would edit the permanent record of what a merge
	// actually applied.
	ErrBranchNotOpen = errors.New("branch is not open for editing")
)

// ConflictError carries BOTH sides of a lost optimistic-concurrency race.
//
// The whole point of surfacing a 409 rather than retrying is that a human has
// to choose, and they cannot choose without seeing what they wrote and what is
// now stored. An error that said only "version conflict" would force the portal
// into a second round trip to render the dialog it must show — and that second
// read could return a third value.
type ConflictError struct {
	// KeyID and Locale identify the cell, so a bulk save can report which of
	// several edits collided.
	KeyID  int64
	Locale string

	// Mine is the value the caller tried to write.
	Mine string

	// Theirs is what is stored now. It is a Cell, not a string, because master
	// may hold no row at all — "someone deleted the translation you were
	// editing" and "someone changed it to blank" are different answers.
	Theirs repository.Cell

	// ExpectedVersion is the base_version the caller sent; Theirs.Version is
	// what it should have been.
	ExpectedVersion int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("optimistic lock conflict on key %d locale %s: sent base_version %d, stored version is %d",
		e.KeyID, e.Locale, e.ExpectedVersion, e.Theirs.Version)
}

// Unwrap ties this to the repository sentinel, so a handler can match either
// the specific type (to build the theirs/mine body) or the general condition.
func (e *ConflictError) Unwrap() error { return repository.ErrOptimisticLock }

// Service reads and writes keys and translations. It owns every transaction
// boundary below; no repository it calls opens one.
type Service struct {
	tx           database.Transactional
	keys         repository.KeyRepository
	translations repository.TranslationRepository
	locales      repository.LocaleRepository
	branches     repository.BranchRepository
	tags         repository.TagRepository
	mrs          repository.MergeRequestRepository
	audit        repository.AuditRepository
}

func ProvideService(
	tx database.Transactional,
	keys repository.KeyRepository,
	translations repository.TranslationRepository,
	locales repository.LocaleRepository,
	branches repository.BranchRepository,
	tags repository.TagRepository,
	mrs repository.MergeRequestRepository,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, keys: keys, translations: translations,
		locales: locales, branches: branches, tags: tags, mrs: mrs, audit: audit}
}

// --- reads ------------------------------------------------------------------

// BrowseRequest is the key browser's query.
type BrowseRequest struct {
	// Branch selects the copy-on-write view. Empty means master.
	Branch string

	// Locales are the codes whose values to return. Empty means every locale.
	Locales []string

	Platform       string
	Tag            string
	Search         string
	UntranslatedIn string
	IncludeDeleted bool

	Limit  int
	Offset int
}

// KeyView is one key as the browser renders it.
type KeyView struct {
	Key  model.Key
	Tags []repository.Tag

	// Values is keyed on locale id and contains an entry for EVERY requested
	// locale, including the untranslated ones. A missing entry would be
	// indistinguishable from "not asked for".
	Values map[int16]repository.Cell

	// BranchModified reports that the branch overrides this key's METADATA.
	// Value overrides are reported per cell by Cell.FromBranch.
	BranchModified bool
}

// BrowseResult is one page of the key browser.
type BrowseResult struct {
	// Branch is nil on master. A pointer rather than a name so a caller cannot
	// mistake the empty string for a branch called "".
	Branch  *repository.Branch
	Locales []model.Locale
	Keys    []KeyView
	Total   int
	Limit   int
	Offset  int
}

// Browse returns a page of keys with their values.
//
// FOUR queries, regardless of how many keys the page holds: the keys, their
// values, their tags, and their branch metadata overrides. The browser fetches
// ~6,300 keys across 6 locales in one request, so a query per key — or per
// locale — would be thousands of round trips for one page.
//
// Deliberately NOT wrapped in a transaction. Four reads outside one can observe
// a write landing between them, which for a browser means a cell rendering one
// version newer than its neighbours. The alternative is holding a snapshot open
// across the largest read in the service, and the inconsistency it would buy is
// smaller than the one the user creates by leaving the page open.
func (s *Service) Browse(ctx context.Context, req BrowseRequest) (BrowseResult, error) {
	var out BrowseResult

	branch, err := s.resolveBranch(ctx, nil, req.Branch)
	if err != nil {
		return out, err
	}
	out.Branch = branch

	locales, err := s.selectLocales(ctx, req.Locales)
	if err != nil {
		return out, err
	}
	out.Locales = locales

	filter := repository.KeyFilter{
		BranchID:       branchID(branch),
		Platform:       req.Platform,
		Tag:            req.Tag,
		Search:         strings.TrimSpace(req.Search),
		IncludeDeleted: req.IncludeDeleted,
		Limit:          req.Limit,
		Offset:         req.Offset,
	}
	if err := validatePlatform(req.Platform); err != nil {
		return out, err
	}
	if req.UntranslatedIn != "" {
		locale, err := s.localeByCode(ctx, nil, req.UntranslatedIn)
		if err != nil {
			return out, err
		}
		filter.UntranslatedIn = locale.ID
	}

	page, err := s.keys.List(ctx, nil, filter)
	if err != nil {
		return out, err
	}

	views, err := s.assemble(ctx, nil, branchID(branch), page.Keys, locales)
	if err != nil {
		return out, err
	}

	out.Keys = views
	out.Total = page.Total
	out.Limit = req.Limit
	out.Offset = req.Offset
	return out, nil
}

// Get returns one key with its values.
func (s *Service) Get(ctx context.Context, keyID int64, branchName string, localeCodes []string) (BrowseResult, error) {
	var out BrowseResult

	branch, err := s.resolveBranch(ctx, nil, branchName)
	if err != nil {
		return out, err
	}
	out.Branch = branch

	locales, err := s.selectLocales(ctx, localeCodes)
	if err != nil {
		return out, err
	}
	out.Locales = locales

	key, err := s.keys.ByID(ctx, nil, keyID)
	if err != nil {
		return out, err
	}

	views, err := s.assemble(ctx, nil, branchID(branch), []model.Key{key}, locales)
	if err != nil {
		return out, err
	}

	out.Keys = views
	out.Total = 1
	return out, nil
}

// assemble fans the three remaining bulk reads out over a page of keys.
//
// Each is ONE query for the whole page — TagsForKeys, ResolveMany and
// KeyMetaForKeys all take an id array. This function is the only place that
// knows a branch overlay exists, so a caller cannot forget to apply it.
func (s *Service) assemble(
	ctx context.Context, tx *gorm.DB, branchID int64, keys []model.Key, locales []model.Locale,
) ([]KeyView, error) {
	views := make([]KeyView, 0, len(keys))
	if len(keys) == 0 {
		return views, nil
	}

	keyIDs := make([]int64, len(keys))
	for i, k := range keys {
		keyIDs[i] = k.ID
	}
	localeIDs := make([]int16, len(locales))
	for i, l := range locales {
		localeIDs[i] = l.ID
	}

	cells, err := s.translations.ResolveMany(ctx, tx, branchID, keyIDs, localeIDs)
	if err != nil {
		return nil, err
	}
	tagsByKey, err := s.tags.TagsForKeys(ctx, tx, keyIDs)
	if err != nil {
		return nil, err
	}
	metaByKey, err := s.branches.KeyMetaForKeys(ctx, tx, branchID, keyIDs)
	if err != nil {
		return nil, err
	}

	for _, k := range keys {
		view := KeyView{Key: k, Tags: tagsByKey[k.ID], Values: make(map[int16]repository.Cell, len(locales))}

		// The branch's metadata wins where it exists. Identity, ordering and
		// master's version are NOT overridden: they belong to the master row,
		// and base_master_version is anchored on master's version, so replacing
		// it here would misreport what the merge will compare.
		if meta, ok := metaByKey[k.ID]; ok {
			view.BranchModified = true
			view.Key.Name = meta.Name
			view.Key.Description = meta.Description
			view.Key.Platforms = meta.Platforms
			view.Key.AndroidName = meta.AndroidName
			view.Key.IOSName = meta.IOSName
			view.Key.Status = meta.Status
		}

		for _, l := range locales {
			// Present for every requested locale, translated or not. An absent
			// entry would be indistinguishable from a locale nobody asked for.
			view.Values[l.ID] = cells[repository.Cell2Key{KeyID: k.ID, LocaleID: l.ID}]
		}
		views = append(views, view)
	}
	return views, nil
}

// History returns a key's metadata and value timeline, newest first.
//
// The two tables are returned separately rather than interleaved: they carry
// different columns, and a merged list would have to flatten both into a
// lowest-common-denominator shape that loses the value three-state on one side
// and the platform set on the other. The portal renders them on one timeline;
// the ordering key is changed_at in both.
//
// localeCode empty means every locale.
func (s *Service) History(
	ctx context.Context, keyID int64, localeCode string, limit int,
) ([]repository.KeyHistoryEntry, []repository.TranslationHistoryEntry, error) {
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}

	// Prove the key exists first, so an unknown id is a 404 rather than two
	// empty lists that read as "nothing ever happened".
	if _, err := s.keys.ByID(ctx, nil, keyID); err != nil {
		return nil, nil, err
	}

	var localeID int16
	if localeCode != "" {
		locale, err := s.localeByCode(ctx, nil, localeCode)
		if err != nil {
			return nil, nil, err
		}
		localeID = locale.ID
	}

	keyEvents, err := s.keys.History(ctx, nil, keyID, limit)
	if err != nil {
		return nil, nil, err
	}
	valueEvents, err := s.translations.History(ctx, nil, keyID, localeID, limit)
	if err != nil {
		return nil, nil, err
	}
	return keyEvents, valueEvents, nil
}

// --- key writes -------------------------------------------------------------

// CreateKeyRequest describes a new key.
type CreateKeyRequest struct {
	Name        string
	Description string
	Platforms   []model.Platform
	AndroidName *string
	IOSName     *string
}

// CreateKey adds a key, on master or on a branch.
//
// ON A BRANCH the key is created in `keys` immediately with status = 'draft',
// plus an ordinary branch_keys delta carrying that key's id and status =
// 'active'. It is NOT withheld from master until merge, and that is the whole
// design rather than a shortcut:
//
//   - branch_translations.key_id is NOT NULL REFERENCES keys (id). Without a
//     real row the key could not carry a single translation, which makes a
//     "key that exists only on the branch" a key nobody can ever fill in.
//   - A draft reaches nobody. Every export reads `keys` WHERE status = 'active'
//     and every release bundle — the OTA payload included — is materialised
//     from exactly those rows, so the key is invisible outside the branch until
//     the merge promotes it.
//   - The merge then needs no new code path: applyKeyMetaSQL already assigns
//     status = bk.status, so draft becomes active alongside every other
//     metadata delta, under the same conflict and collision checks.
//
// The name check below is a courtesy that fails fast. The authority is
// NameCollisions inside the merge transaction, which is re-computed under the
// advisory lock — master can gain the name at any point after this returns, and
// only the merge is in a position to say so.
func (s *Service) CreateKey(
	ctx context.Context, branchName string, req CreateKeyRequest, actor, requestID string,
) (model.Key, error) {
	var created model.Key

	name, err := validateName(req.Name)
	if err != nil {
		return created, err
	}
	if err := validateDescription(req.Description); err != nil {
		return created, err
	}
	platforms, err := validatePlatforms(req.Platforms)
	if err != nil {
		return created, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.resolveOpenBranch(ctx, tx, branchName)
		if err != nil {
			return err
		}

		status := model.KeyStatusActive
		if branch != nil {
			status = model.KeyStatusDraft

			// createKeySQL's ON CONFLICT target is the PARTIAL index over active
			// names, and a draft is not active — so nothing in the statement
			// would refuse a draft duplicating a live key's name. Ask directly.
			taken, err := s.keys.IDsByName(ctx, tx, []string{name})
			if err != nil {
				return err
			}
			if _, exists := taken[name]; exists {
				return fmt.Errorf("create key %q on branch %q: %w",
					name, branch.Name, repository.ErrKeyNameTaken)
			}
		}

		k, err := s.keys.Create(ctx, tx, model.Key{
			Name:        name,
			Description: strings.TrimSpace(req.Description),
			Platforms:   platforms,
			AndroidName: req.AndroidName,
			IOSName:     req.IOSName,
			Status:      status,
		})
		if err != nil {
			return err
		}
		created = k

		if branch != nil {
			// The delta says 'active': it is the instruction the merge carries
			// out. Two branches may each hold a draft of the same name — the
			// partial index does not stop them — and whichever merges second is
			// refused by NameCollisions rather than dying on the index.
			onBranch := k
			onBranch.Status = model.KeyStatusActive
			if err := s.branches.SetKeyMeta(ctx, tx, branch.ID, k.ID, onBranch, actor); err != nil {
				return err
			}
			if err := s.afterBranchWrite(ctx, tx, branch.ID); err != nil {
				return err
			}
			created = onBranch
		}

		var historyBranch *int64
		if branch != nil {
			historyBranch = &branch.ID
		}
		if err := s.keys.RecordHistory(ctx, tx, created, model.SourceUI, historyBranch, actor); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:  actor,
			Action: repository.ActionKeyCreate,
			Target: fmt.Sprintf("key:%d", k.ID),
			Metadata: map[string]any{
				"name":      k.Name,
				"platforms": platformStrings(k.Platforms),
				"branch":    branchName,
			},
			RequestID: requestID,
		})
	})
	if err != nil {
		return model.Key{}, err
	}

	log.Infow(ctx, "key created", "key_id", created.ID, "name", created.Name,
		"branch", branchName, "actor", actor)
	return created, nil
}

// UpdateKeyRequest is a partial update.
//
// Every field is a pointer, and the two override columns carry an explicit
// Clear flag as well. That is the three-state rule applied to a request body:
// "leave it alone", "set it to this" and "set it back to NULL" are three
// different instructions, and a plain *string can only express two of them.
// android_name NULL means "derive from the key name", which is a real and
// unrecoverable distinction from any particular string.
type UpdateKeyRequest struct {
	KeyID  int64
	Branch string

	Name        *string
	Description *string
	Platforms   []model.Platform

	AndroidName      *string
	ClearAndroidName bool
	IOSName          *string
	ClearIOSName     bool

	// BaseVersion applies optimistic concurrency control when non-nil. Unlike a
	// translation edit it is optional: a key's metadata form is opened
	// deliberately and saved once, where a grid of 38,000 cells is not.
	BaseVersion *int
}

// UpdateKey rewrites a key's metadata, on master or as a branch delta.
func (s *Service) UpdateKey(
	ctx context.Context, req UpdateKeyRequest, actor, requestID string,
) (model.Key, error) {
	var updated model.Key

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.resolveOpenBranch(ctx, tx, req.Branch)
		if err != nil {
			return err
		}

		master, err := s.keys.ByID(ctx, tx, req.KeyID)
		if err != nil {
			return err
		}

		// The base being patched is the branch's own delta when it has one:
		// patching master's copy instead would silently revert every field the
		// branch had already changed and the caller did not mention.
		base := master
		if branch != nil {
			metas, err := s.branches.KeyMetaForKeys(ctx, tx, branch.ID, []int64{req.KeyID})
			if err != nil {
				return err
			}
			if meta, ok := metas[req.KeyID]; ok {
				meta.ID = master.ID
				meta.Version = master.Version
				meta.SortIndex = master.SortIndex
				base = meta
			}
		}

		patched, err := applyKeyPatch(base, req)
		if err != nil {
			return err
		}

		if branch != nil {
			if err := s.branches.SetKeyMeta(ctx, tx, branch.ID, req.KeyID, patched, actor); err != nil {
				return err
			}
			if err := s.afterBranchWrite(ctx, tx, branch.ID); err != nil {
				return err
			}
			// The version recorded is MASTER's: a branch delta does not move
			// keys.version, and writing a fabricated number into an append-only
			// table would make the timeline unreadable.
			patched.Version = master.Version
			updated = patched
			if err := s.keys.RecordHistory(ctx, tx, patched, model.SourceUI, &branch.ID, actor); err != nil {
				return err
			}
		} else {
			expected := 0
			if req.BaseVersion != nil {
				expected = *req.BaseVersion
			}
			k, err := s.keys.Update(ctx, tx, patched, expected)
			if err != nil {
				return err
			}
			updated = k
			if err := s.keys.RecordHistory(ctx, tx, k, model.SourceUI, nil, actor); err != nil {
				return err
			}
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionKeyUpdate,
			Target:    fmt.Sprintf("key:%d", req.KeyID),
			Metadata:  map[string]any{"branch": req.Branch, "name": updated.Name},
			RequestID: requestID,
		})
	})
	if err != nil {
		return model.Key{}, err
	}

	log.Infow(ctx, "key updated", "key_id", updated.ID, "branch", req.Branch, "actor", actor)
	return updated, nil
}

// DeleteKey soft-deletes a key: status becomes 'deleted' and the row survives.
//
// Never a DELETE. History and audit rows reference the key, translations
// cascade from it, and idx_keys_name_active is partial precisely so that a
// deleted key stops reserving its name — all three depend on the row remaining.
func (s *Service) DeleteKey(
	ctx context.Context, keyID int64, branchName string, baseVersion *int, actor, requestID string,
) (model.Key, error) {
	var deleted model.Key

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.resolveOpenBranch(ctx, tx, branchName)
		if err != nil {
			return err
		}

		master, err := s.keys.ByID(ctx, tx, keyID)
		if err != nil {
			return err
		}

		if branch != nil {
			marked := master
			marked.Status = model.KeyStatusDeleted
			if err := s.branches.SetKeyMeta(ctx, tx, branch.ID, keyID, marked, actor); err != nil {
				return err
			}
			if err := s.afterBranchWrite(ctx, tx, branch.ID); err != nil {
				return err
			}
			deleted = marked
			if err := s.keys.RecordHistory(ctx, tx, marked, model.SourceUI, &branch.ID, actor); err != nil {
				return err
			}
		} else {
			expected := 0
			if baseVersion != nil {
				expected = *baseVersion
			}
			k, err := s.keys.SoftDelete(ctx, tx, keyID, expected)
			if err != nil {
				return err
			}
			deleted = k
			if err := s.keys.RecordHistory(ctx, tx, k, model.SourceUI, nil, actor); err != nil {
				return err
			}
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionKeyDelete,
			Target:    fmt.Sprintf("key:%d", keyID),
			Metadata:  map[string]any{"branch": branchName, "name": deleted.Name},
			RequestID: requestID,
		})
	})
	if err != nil {
		return model.Key{}, err
	}

	log.Infow(ctx, "key deleted", "key_id", keyID, "branch", branchName, "actor", actor)
	return deleted, nil
}

// --- translation writes -----------------------------------------------------

// SetTranslationRequest is one inline cell edit.
type SetTranslationRequest struct {
	KeyID      int64
	LocaleCode string
	Branch     string

	Value      string
	RenderHint model.RenderHint

	// BaseVersion is the version the editor was shown. It is REQUIRED on
	// master: without it every save is a blind last-write-wins, and two
	// translators on one string means one of them loses their work with no
	// indication it happened. Zero is a legitimate value meaning "there was no
	// row when I started".
	//
	// It must NOT be sent on a branch write. branch_translations has no version
	// column to guard against — a branch is a private workspace whose collisions
	// with master are detected once, at merge, against base_master_version.
	// Accepting the field there and ignoring it would be worse than refusing it:
	// the portal would believe it had concurrency control that does not exist.
	BaseVersion *int
}

// SetTranslation writes one value, applying optimistic concurrency control.
func (s *Service) SetTranslation(
	ctx context.Context, req SetTranslationRequest, actor, requestID string,
) (repository.Cell, error) {
	var result repository.Cell

	if len(req.Value) > maxValueLen {
		return result, fmt.Errorf("%w: value must be at most %d characters, got %d",
			ErrBadRequest, maxValueLen, len(req.Value))
	}
	hint, err := validateRenderHint(req.RenderHint)
	if err != nil {
		return result, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.resolveOpenBranch(ctx, tx, req.Branch)
		if err != nil {
			return err
		}
		locale, err := s.localeByCode(ctx, tx, req.LocaleCode)
		if err != nil {
			return err
		}
		key, err := s.keys.ByID(ctx, tx, req.KeyID)
		if err != nil {
			return err
		}
		if key.Status == model.KeyStatusDeleted {
			return fmt.Errorf("%w: key %d is deleted", ErrBadRequest, req.KeyID)
		}

		if branch != nil {
			if req.BaseVersion != nil {
				return fmt.Errorf(
					"%w: base_version does not apply to a branch edit — branch changes are reconciled against master once, at merge",
					ErrBadRequest)
			}
			if err := s.branches.SetValue(ctx, tx, branch.ID, req.KeyID, locale.ID,
				req.Value, hint, actor); err != nil {
				return err
			}
			if err := s.afterBranchWrite(ctx, tx, branch.ID); err != nil {
				return err
			}

			resolved, err := s.branches.Resolve(ctx, tx, branch.ID, req.KeyID, locale.ID)
			if err != nil {
				return err
			}
			result = repository.Cell{
				Value: resolved.Value, Found: resolved.Found, RenderHint: hint,
				Version: resolved.MasterVersion, FromBranch: resolved.FromDelta,
				UpdatedBy: actor,
			}

			value := req.Value
			return s.translations.RecordHistory(ctx, tx, req.KeyID, locale.ID, &value,
				hint, resolved.MasterVersion, model.SourceUI, &branch.ID, actor)
		}

		if req.BaseVersion == nil {
			return fmt.Errorf(
				"%w: base_version is required — send the version you were shown, or 0 if the value was untranslated",
				ErrBadRequest)
		}
		expected := *req.BaseVersion

		current, err := s.translations.GetCell(ctx, tx, req.KeyID, locale.ID)
		if err != nil {
			return err
		}

		// The comparison below is a courtesy: it produces a 409 whose body
		// already carries both values. The guard that actually holds is the
		// version predicate inside CreateCell and UpdateWithVersion, which runs
		// in the same statement as the write. Anything checked before a write is
		// stale by the time the write happens.
		if current.Version != expected {
			return &ConflictError{KeyID: req.KeyID, Locale: locale.Code,
				Mine: req.Value, Theirs: current, ExpectedVersion: expected}
		}

		write := model.Translation{KeyID: req.KeyID, LocaleID: locale.ID,
			Value: req.Value, RenderHint: hint, UpdatedBy: actor}

		if !current.Found {
			err = s.translations.CreateCell(ctx, tx, write)
		} else {
			err = s.translations.UpdateWithVersion(ctx, tx, write, expected)
		}
		if err != nil {
			if errors.Is(err, repository.ErrOptimisticLock) {
				// Lost the race between the read above and the write. Re-read so
				// the caller sees what actually won, not what we saw first.
				theirs, readErr := s.translations.GetCell(ctx, tx, req.KeyID, locale.ID)
				if readErr != nil {
					return readErr
				}
				return &ConflictError{KeyID: req.KeyID, Locale: locale.Code,
					Mine: req.Value, Theirs: theirs, ExpectedVersion: expected}
			}
			return err
		}

		newVersion := current.Version + 1
		result = repository.Cell{Value: req.Value, Found: true, RenderHint: hint,
			Version: newVersion, UpdatedBy: actor}

		value := req.Value
		return s.translations.RecordHistory(ctx, tx, req.KeyID, locale.ID, &value,
			hint, newVersion, model.SourceUI, nil, actor)
	})
	if err != nil {
		return repository.Cell{}, err
	}

	log.Infow(ctx, "translation written", "key_id", req.KeyID,
		"locale", req.LocaleCode, "branch", req.Branch, "actor", actor)
	return result, nil
}

// DeleteTranslation removes the row, making the pair UNTRANSLATED.
//
// Emphatically not "set it to empty". An absent row is omitted from the export
// under skip_empty and is what "nobody has translated this yet" means; a row
// holding "" is a deliberate blank that exports as "". A DELETE that wrote ""
// would quietly convert ~430 untranslated en-SG keys into intentional blanks.
func (s *Service) DeleteTranslation(
	ctx context.Context, keyID int64, localeCode, branchName string,
	baseVersion *int, actor, requestID string,
) error {
	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		branch, err := s.resolveOpenBranch(ctx, tx, branchName)
		if err != nil {
			return err
		}
		locale, err := s.localeByCode(ctx, tx, localeCode)
		if err != nil {
			return err
		}
		if _, err := s.keys.ByID(ctx, tx, keyID); err != nil {
			return err
		}

		if branch != nil {
			if baseVersion != nil {
				return fmt.Errorf(
					"%w: base_version does not apply to a branch edit — branch changes are reconciled against master once, at merge",
					ErrBadRequest)
			}

			resolved, err := s.branches.Resolve(ctx, tx, branch.ID, keyID, locale.ID)
			if err != nil {
				return err
			}
			if !resolved.Found {
				// Already untranslated on this branch. A tombstone over nothing
				// is a delta that means nothing, and answering 204 would tell the
				// caller they removed something that was never there.
				return fmt.Errorf("translation (%d,%s) on branch %s: %w",
					keyID, locale.Code, branch.Name, repository.ErrNotFound)
			}

			// A TOMBSTONE, not a delta holding "". The branch says "this
			// translation goes away at merge", and removeDeltasSQL turns it into
			// a DELETE against master rather than a blanking UPDATE.
			if err := s.branches.RemoveValue(ctx, tx, branch.ID, keyID, locale.ID, actor); err != nil {
				return err
			}
			if err := s.afterBranchWrite(ctx, tx, branch.ID); err != nil {
				return err
			}

			// NULL, not "": the history row must say the pair became
			// untranslated rather than blank, or the timeline loses the same
			// distinction the table does.
			return s.translations.RecordHistory(ctx, tx, keyID, locale.ID, nil,
				model.RenderHintPlain, resolved.MasterVersion, model.SourceUI, &branch.ID, actor)
		}

		current, err := s.translations.GetCell(ctx, tx, keyID, locale.ID)
		if err != nil {
			return err
		}
		if !current.Found {
			return fmt.Errorf("translation (%d,%s): %w", keyID, locale.Code, repository.ErrNotFound)
		}
		if baseVersion != nil && *baseVersion != current.Version {
			return &ConflictError{KeyID: keyID, Locale: locale.Code,
				Theirs: current, ExpectedVersion: *baseVersion}
		}

		if err := s.translations.Delete(ctx, tx, keyID, locale.ID); err != nil {
			return err
		}

		// The version recorded is the one the deleted row held. There is no new
		// version to report — the row is gone — and inventing one would claim a
		// state that never existed.
		return s.translations.RecordHistory(ctx, tx, keyID, locale.ID, nil,
			current.RenderHint, current.Version, model.SourceUI, nil, actor)
	})
	if err != nil {
		return err
	}

	log.Infow(ctx, "translation removed", "key_id", keyID,
		"locale", localeCode, "branch", branchName, "actor", actor)
	return nil
}

// --- helpers ----------------------------------------------------------------

// afterBranchWrite re-opens an approved merge request whose branch just changed.
//
// Every delta write must call it. SetValue and SetKeyMeta already bump
// branches.last_edited_at; this is what turns that timestamp into the visible
// consequence — a reviewer cannot approve one diff and have a different one
// merged. The merge transaction checks again defensively, and that second check
// is the one that actually protects, because anything checked before the
// transaction is already stale.
func (s *Service) afterBranchWrite(ctx context.Context, tx *gorm.DB, branchID int64) error {
	invalidated, err := s.mrs.InvalidateApprovalIfEdited(ctx, tx, branchID)
	if err != nil {
		return err
	}
	if invalidated {
		log.Infow(ctx, "approval invalidated by branch edit", "branch_id", branchID)
	}
	return nil
}

// resolveBranch turns a branch name into a row. An empty name means master and
// yields nil — never a zero-valued Branch, which would read as a branch called
// "" with id 0.
func (s *Service) resolveBranch(ctx context.Context, tx *gorm.DB, name string) (*repository.Branch, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	b, err := s.branches.ByName(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// resolveOpenBranch additionally refuses a branch that is merged or closed.
func (s *Service) resolveOpenBranch(ctx context.Context, tx *gorm.DB, name string) (*repository.Branch, error) {
	b, err := s.resolveBranch(ctx, tx, name)
	if err != nil || b == nil {
		return b, err
	}
	if b.Status != repository.BranchStatusOpen {
		return nil, fmt.Errorf("%w: branch %q is %s", ErrBranchNotOpen, b.Name, b.Status)
	}
	return b, nil
}

func branchID(b *repository.Branch) int64 {
	if b == nil {
		return 0
	}
	return b.ID
}

// selectLocales resolves codes to rows, rejecting anything unknown.
//
// Rejecting rather than silently returning a subset: a portal that misspells a
// locale must be told, not handed five columns where it asked for six and left
// to conclude the sixth is untranslated everywhere.
func (s *Service) selectLocales(ctx context.Context, codes []string) ([]model.Locale, error) {
	all, err := s.locales.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(codes) == 0 {
		return all, nil
	}

	byCode := make(map[string]model.Locale, len(all))
	for _, l := range all {
		byCode[l.Code] = l
	}

	out := make([]model.Locale, 0, len(codes))
	var unknown []string
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		l, ok := byCode[code]
		if !ok {
			unknown = append(unknown, code)
			continue
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, l)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%w: unknown locales %v", ErrBadRequest, unknown)
	}
	return out, nil
}

func (s *Service) localeByCode(ctx context.Context, tx *gorm.DB, code string) (model.Locale, error) {
	l, err := s.locales.ByCode(ctx, tx, code)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// A locale is reference data seeded by migration, so an unknown code
			// is a caller mistake rather than a missing resource.
			return l, fmt.Errorf("%w: unknown locale %q", ErrBadRequest, code)
		}
		return l, err
	}
	return l, nil
}

// applyKeyPatch folds a partial update onto the current metadata.
func applyKeyPatch(base model.Key, req UpdateKeyRequest) (model.Key, error) {
	out := base

	if req.Name != nil {
		name, err := validateName(*req.Name)
		if err != nil {
			return out, err
		}
		out.Name = name
	}
	if req.Description != nil {
		if err := validateDescription(*req.Description); err != nil {
			return out, err
		}
		out.Description = strings.TrimSpace(*req.Description)
	}
	if req.Platforms != nil {
		platforms, err := validatePlatforms(req.Platforms)
		if err != nil {
			return out, err
		}
		out.Platforms = platforms
	}

	switch {
	case req.ClearAndroidName:
		// Back to NULL — "derive from the key name". A distinct instruction
		// from setting any particular string, and unrecoverable if collapsed.
		out.AndroidName = nil
	case req.AndroidName != nil:
		out.AndroidName = req.AndroidName
	}
	switch {
	case req.ClearIOSName:
		out.IOSName = nil
	case req.IOSName != nil:
		out.IOSName = req.IOSName
	}

	return out, nil
}

// validateName enforces what the schema cannot.
//
// keys.name has no CHECK, but a name with surrounding whitespace produces a key
// that looks identical to another one in every UI and collides with nothing —
// the worst possible failure, because it is invisible.
func validateName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name is required", ErrBadRequest)
	}
	if trimmed != name {
		return "", fmt.Errorf("%w: name must not begin or end with whitespace", ErrBadRequest)
	}
	if len(name) > maxNameLen {
		return "", fmt.Errorf("%w: name must be at most %d characters, got %d",
			ErrBadRequest, maxNameLen, len(name))
	}
	if strings.ContainsAny(name, "\n\r\t") {
		return "", fmt.Errorf("%w: name must not contain control characters", ErrBadRequest)
	}
	return name, nil
}

func validateDescription(description string) error {
	if len(description) > maxDescriptionLen {
		return fmt.Errorf("%w: description must be at most %d characters, got %d",
			ErrBadRequest, maxDescriptionLen, len(description))
	}
	return nil
}

// validatePlatforms mirrors keys_platforms_check.
//
// Duplicated from the constraint deliberately: the database is the last line of
// defence and keeps its check, but a constraint violation surfaces as a 500 with
// a Postgres string in it, and an unknown platform is a typo.
func validatePlatforms(platforms []model.Platform) ([]model.Platform, error) {
	if len(platforms) == 0 {
		return nil, fmt.Errorf("%w: at least one platform is required (flutter, android, ios)", ErrBadRequest)
	}

	seen := make(map[model.Platform]bool, len(platforms))
	out := make([]model.Platform, 0, len(platforms))
	for _, p := range platforms {
		switch p {
		case model.PlatformFlutter, model.PlatformAndroid, model.PlatformIOS:
		default:
			return nil, fmt.Errorf("%w: unknown platform %q, want flutter, android or ios",
				ErrBadRequest, p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

func validatePlatform(platform string) error {
	if platform == "" {
		return nil
	}
	_, err := validatePlatforms([]model.Platform{model.Platform(platform)})
	return err
}

// validateRenderHint mirrors translations_render_hint_check.
func validateRenderHint(hint model.RenderHint) (model.RenderHint, error) {
	switch hint {
	case "":
		return model.RenderHintPlain, nil
	case model.RenderHintPlain, model.RenderHintCDATA:
		return hint, nil
	default:
		return "", fmt.Errorf("%w: render_hint must be plain or cdata, got %q", ErrBadRequest, hint)
	}
}

func platformStrings(platforms []model.Platform) []string {
	out := make([]string, len(platforms))
	for i, p := range platforms {
		out[i] = string(p)
	}
	return out
}
