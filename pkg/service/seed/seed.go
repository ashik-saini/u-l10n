// Package seed loads the committed u-mobile localization tree into the
// database.
//
// This is the disaster-recovery path, the drift-reconciliation tool at cutover,
// and — most usefully right now — the reason the whole store and export engine
// can be built and proven BEFORE anyone obtains a Lokalise API token.
//
// # Which files are authoritative
//
// Values come only from the Flutter JSON files. They carry CANONICAL key names
// and map 1:1 onto locales, so there is no ambiguity about what a value belongs
// to.
//
// The Android and iOS trees are used only to establish platform MEMBERSHIP, for
// two reasons. First, run.sh fans one exported directory out to several
// repository directories, so the mapping back to a locale is many-to-one.
// Second, and decisively, those fan-out targets have drifted apart: the three
// en-SG iOS targets hold 929, 4897 and 3227 keys, and values/ and values-en/
// hold 3199 and 3149. Picking one as authoritative would be arbitrary, so
// membership is taken as the UNION across every committed file for that
// platform. That is also the more useful answer for the export filter.
package seed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/export"
	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/parse"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// Options configures a seed run.
type Options struct {
	// Root is the u-mobile repository root.
	Root string
	// Actor is recorded as updated_by. Not optional: an unattributed write to
	// customer-facing copy is not acceptable.
	Actor string
	// DryRun executes the entire real pipeline inside a transaction and then
	// rolls it back. It is the highest-confidence rehearsal available, because
	// it exercises every constraint and conflict for real and leaves nothing
	// behind — strictly better than a "simulate" code path, which would be a
	// second implementation that can itself be wrong.
	DryRun bool
}

// Result reports what a run did, or would have done.
type Result struct {
	DryRun              bool
	LocalesSeen         int
	KeysUpserted        int
	TranslationsWritten int
	// Warnings are non-fatal observations that a human should see: duplicate
	// keys collapsed, Android names with no canonical match, files missing.
	Warnings []string
}

// errDryRun aborts the transaction after a successful dry run. Returning an
// error is how WithTransaction is told to roll back; it never reaches a caller.
var errDryRun = errors.New("dry run: rolling back")

// Service performs seed runs.
type Service struct {
	tx           database.Transactional
	locales      repository.LocaleRepository
	keys         repository.KeyRepository
	translations repository.TranslationRepository
}

func ProvideService(
	tx database.Transactional,
	locales repository.LocaleRepository,
	keys repository.KeyRepository,
	translations repository.TranslationRepository,
) *Service {
	return &Service{tx: tx, locales: locales, keys: keys, translations: translations}
}

// Run seeds the database from the tree at opts.Root.
//
// The whole run is ONE transaction. The service owns that boundary and threads
// tx into every repository call, because the shared WithTransaction helper does
// not nest — a repository opening its own transaction would silently split the
// work and destroy the all-or-nothing property this relies on.
func (s *Service) Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Root == "" {
		return nil, errors.New("seed: root path is required")
	}
	if opts.Actor == "" {
		return nil, errors.New("seed: actor is required; writes to customer copy must be attributable")
	}

	result := &Result{DryRun: opts.DryRun}

	err := s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.run(ctx, tx, opts, result); err != nil {
			return err
		}
		if opts.DryRun {
			return errDryRun
		}
		return nil
	})

	// A dry run that reached the end is a SUCCESS whose rollback we asked for.
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, err
	}

	log.Infow(ctx, "seed complete",
		"dry_run", opts.DryRun, "locales", result.LocalesSeen,
		"keys", result.KeysUpserted, "translations", result.TranslationsWritten,
		"warnings", len(result.Warnings))

	return result, nil
}

func (s *Service) run(ctx context.Context, tx *gorm.DB, opts Options, result *Result) error {
	locales, err := s.locales.List(ctx, tx)
	if err != nil {
		return err
	}
	if len(locales) == 0 {
		return errors.New("seed: no locales; run migrations first")
	}
	result.LocalesSeen = len(locales)

	// Pass 1 — read every Flutter file. Values and canonical key order come
	// from here.
	parsed := make(map[string]*parse.File, len(locales))
	for _, locale := range locales {
		path := filepath.Join(opts.Root, "assets", "langs", locale.Code+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		file, err := parse.JSONBytes(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		parsed[locale.Code] = file

		for key, values := range file.Duplicates() {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: key %q appears %d times; taking the last value, matching what the app does",
				locale.Code, key, len(values)))
		}
	}

	// Canonical key order: first-seen across locales in locale sort order. The
	// export must reproduce Lokalise's ordering, so order is data.
	var order []string
	seen := make(map[string]bool)
	platforms := make(map[string]map[model.Platform]bool)

	for _, locale := range locales {
		for _, e := range parsed[locale.Code].Entries {
			if !seen[e.Key] {
				seen[e.Key] = true
				order = append(order, e.Key)
			}
			if platforms[e.Key] == nil {
				platforms[e.Key] = map[model.Platform]bool{}
			}
			platforms[e.Key][model.PlatformFlutter] = true
		}
	}

	// Pass 2 — platform membership from the Android and iOS trees.
	s.addAndroidMembership(opts.Root, order, platforms, result)
	s.addIOSMembership(opts.Root, platforms, result)

	// Pass 3 — upsert keys, assigning gapped sort indexes so a later insert
	// between two keys is one UPDATE rather than a renumber of 6,000 rows.
	base, err := s.keys.MaxSortIndex(ctx, tx)
	if err != nil {
		return err
	}

	for i, name := range order {
		var list []model.Platform
		for p := range platforms[name] {
			list = append(list, p)
		}
		sort.Slice(list, func(a, b int) bool { return list[a] < list[b] })

		if _, err := s.keys.UpsertByName(ctx, tx, model.Key{
			Name:      name,
			Platforms: list,
			Status:    model.KeyStatusActive,
			SortIndex: base + int64(i+1)*100,
		}); err != nil {
			return err
		}
		result.KeysUpserted++
	}

	ids, err := s.keys.IDsByName(ctx, tx, order)
	if err != nil {
		return err
	}

	// Pass 4 — write values. One batch per locale: 36,000 individual round
	// trips would dominate the runtime.
	for _, locale := range locales {
		file := parsed[locale.Code]

		// Collapse duplicates last-wins BEFORE writing. The database cannot
		// hold two values for one (key, locale), and last-wins is what both
		// Dart's json.decode and the iOS loader do at runtime.
		latest := make(map[string]string, file.Len())
		var keyOrder []string
		for _, e := range file.Entries {
			if _, ok := latest[e.Key]; !ok {
				keyOrder = append(keyOrder, e.Key)
			}
			latest[e.Key] = e.Value
		}

		batch := make([]model.Translation, 0, len(keyOrder))
		for _, key := range keyOrder {
			id, ok := ids[key]
			if !ok {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"%s: key %q has no row after upsert; skipped", locale.Code, key))
				continue
			}
			batch = append(batch, model.Translation{
				KeyID:      id,
				LocaleID:   locale.ID,
				Value:      latest[key],
				RenderHint: model.RenderHintPlain,
				UpdatedBy:  opts.Actor,
			})
		}

		if err := s.translations.UpsertBatch(ctx, tx, batch); err != nil {
			return err
		}
		result.TranslationsWritten += len(batch)
	}

	return nil
}

// androidDirs are every committed values-* directory holding app copy. The
// dpi-qualified directories are excluded: they carry dimensions, not strings.
var androidDirs = []string{
	"values", "values-ab", "values-am", "values-au",
	"values-en", "values-en-rMY", "values-ms", "values-th",
}

// addAndroidMembership marks a key as an Android key when its TRANSFORMED name
// appears in any committed strings.xml.
//
// The Android files store transformed names, so the canonical name cannot be
// recovered from them directly — the transform is lossy ('a-b' and 'a_b' both
// become 'a_b'). Instead the transform is applied FORWARD to each canonical key
// and the result looked up, which is well-defined.
func (s *Service) addAndroidMembership(
	root string, order []string, platforms map[string]map[model.Platform]bool, result *Result,
) {
	present := make(map[string]bool)
	for _, dir := range androidDirs {
		path := filepath.Join(root, "android", "app", "src", "main", "res", dir, "strings.xml")
		data, err := os.ReadFile(path)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("android: %s unreadable, skipped: %v", dir, err))
			continue
		}
		file, err := parse.XMLBytes(data)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("android: %s unparseable, skipped: %v", dir, err))
			continue
		}
		for _, e := range file.Entries {
			present[e.Key] = true
		}
	}

	var matched int
	for _, name := range order {
		if present[export.AndroidName(name)] {
			platforms[name][model.PlatformAndroid] = true
			matched++
		}
	}
	log.Infow(context.Background(), "android membership resolved",
		"android_names_seen", len(present), "canonical_keys_matched", matched)
}

// iosLprojs are every committed .lproj holding app copy, plus the root file.
var iosLprojs = []string{
	"Localizable.strings",
	"Base.lproj/Localizable.strings",
	"en.lproj/Localizable.strings",
	"en-MY.lproj/Localizable.strings",
	"en-TH.lproj/Localizable.strings",
	"en-AU.lproj/Localizable.strings",
	"ms.lproj/Localizable.strings",
	"th.lproj/Localizable.strings",
}

// addIOSMembership marks a key as an iOS key when it appears in any committed
// .strings file. iOS keys are NOT transformed, so these are canonical names and
// match directly.
func (s *Service) addIOSMembership(
	root string, platforms map[string]map[model.Platform]bool, result *Result,
) {
	for _, rel := range iosLprojs {
		path := filepath.Join(root, "ios", "Runner", rel)
		data, err := os.ReadFile(path)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("ios: %s unreadable, skipped: %v", rel, err))
			continue
		}
		file, err := parse.Strings(data)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("ios: %s unparseable, skipped: %v", rel, err))
			continue
		}
		for _, e := range file.Entries {
			// Only mark keys we already know from the Flutter files. An iOS-only
			// key would need a canonical row of its own, which is a separate
			// decision the importer should make deliberately rather than here.
			if _, known := platforms[e.Key]; known {
				platforms[e.Key][model.PlatformIOS] = true
			}
		}
	}
}
