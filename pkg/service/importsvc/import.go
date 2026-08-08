// Package importsvc imports a Lokalise project into u-l10n.
//
// The API alone is not sufficient. It returns "" both for a deliberately-blank
// translation and for one nobody has translated, and those are different facts:
// blank exports as "" while untranslated is omitted entirely. Collapsing them
// would add ~430 spurious keys to en-SG and delete 3,664 intentional blanks
// from ms-MY.
//
// So the import reads TWO sources and takes what each actually knows:
//
//	API           the value of every translation
//	file exports  WHICH keys are present per locale — the presence oracle
//
// Neither source is trusted beyond its competence.
package importsvc

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

	"github.com/yougroupteam/u-l10n/pkg/lokalise"
	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/parse"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// Fetcher is the slice of the Lokalise client this service needs. Depending on
// the interface rather than the concrete client keeps the tests free of HTTP.
type Fetcher interface {
	Languages(ctx context.Context) ([]lokalise.Language, error)
	Keys(ctx context.Context) ([]lokalise.Key, error)
}

// Options configures an import run.
type Options struct {
	// ExportRoot is a directory of Lokalise file exports, used ONLY as the
	// presence oracle. Required: without it the import cannot tell blank from
	// untranslated, and would corrupt thousands of values.
	ExportRoot string
	Actor      string
	// DryRun executes the real pipeline inside a transaction and rolls back.
	DryRun bool
}

// Result reports what a run did, or would have done.
type Result struct {
	DryRun              bool
	KeysUpserted        int
	TranslationsWritten int
	TranslationsSkipped int
	Warnings            []string
}

var errDryRun = errors.New("dry run: rolling back")

// Service performs imports.
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

// Run imports the project.
//
// Idempotent: keys upsert on lokalise_key_id, so an interrupted run is re-run
// rather than cleaned up by hand. That property is what turns "did that
// half-finished import corrupt anything?" into a question nobody has to ask.
func (s *Service) Run(ctx context.Context, client Fetcher, opts Options) (*Result, error) {
	if opts.ExportRoot == "" {
		return nil, errors.New("import: export root is required as the presence oracle; " +
			"the API cannot distinguish an empty translation from an absent one")
	}
	if opts.Actor == "" {
		return nil, errors.New("import: actor is required; writes to customer copy must be attributable")
	}

	result := &Result{DryRun: opts.DryRun}

	// Fetch OUTSIDE the transaction. A minute of network time is not something
	// to hold a database transaction open across.
	languages, err := client.Languages(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch languages: %w", err)
	}
	remoteKeys, err := client.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch keys: %w", err)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.run(ctx, tx, languages, remoteKeys, opts, result); err != nil {
			return err
		}
		if opts.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, err
	}

	log.Infow(ctx, "import complete", "dry_run", opts.DryRun,
		"keys", result.KeysUpserted, "translations", result.TranslationsWritten,
		"skipped", result.TranslationsSkipped, "warnings", len(result.Warnings))
	return result, nil
}

func (s *Service) run(
	ctx context.Context, tx *gorm.DB,
	languages []lokalise.Language, remoteKeys []lokalise.Key,
	opts Options, result *Result,
) error {
	locales, err := s.locales.List(ctx, tx)
	if err != nil {
		return err
	}
	if len(locales) == 0 {
		return errors.New("import: no locales; run migrations first")
	}

	// Validate the remote project matches our locale set EXACTLY. A locale we
	// do not know would be imported into nothing; one we know that is missing
	// remotely would silently produce an empty column.
	remote := make(map[string]bool, len(languages))
	for _, l := range languages {
		remote[l.LangISO] = true
	}
	for _, l := range locales {
		if !remote[l.Code] {
			return fmt.Errorf("import: locale %q is configured here but absent from the project", l.Code)
		}
	}
	for code := range remote {
		if !hasLocale(locales, code) {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("project has locale %q which u-l10n does not; its values are ignored", code))
		}
	}

	// Load the presence oracle: which keys each locale's export actually
	// contains.
	present, err := s.loadPresence(opts.ExportRoot, locales, result)
	if err != nil {
		return err
	}

	base, err := s.keys.MaxSortIndex(ctx, tx)
	if err != nil {
		return err
	}

	// Stable order so sort_index is reproducible across runs: a re-import must
	// not reshuffle the export.
	sort.Slice(remoteKeys, func(i, j int) bool { return remoteKeys[i].KeyID < remoteKeys[j].KeyID })

	byLocale := make(map[string][]model.Translation, len(locales))

	for i, rk := range remoteKeys {
		name := rk.KeyName.Canonical()
		if name == "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("lokalise key %d has no name on any platform; skipped", rk.KeyID))
			continue
		}

		keyID, err := s.keys.UpsertByName(ctx, tx, model.Key{
			Name:        name,
			Description: rk.Description,
			Platforms:   mapPlatforms(rk.Platforms),
			Status:      model.KeyStatusActive,
			// Seeded from the Lokalise id so export order reproduces theirs.
			SortIndex:     base + int64(i+1)*100,
			LokaliseKeyID: &rk.KeyID,
		})
		if err != nil {
			return err
		}
		result.KeysUpserted++

		for _, t := range rk.Translations {
			locale, ok := localeByCode(locales, t.LangISO)
			if !ok {
				continue // already warned above
			}

			// THE PRESENCE DECISION. The API says "" — the export says whether
			// that means "deliberately blank" or "nobody translated it". Only
			// write a row when the export contains the key.
			if !present[t.LangISO][name] {
				result.TranslationsSkipped++
				continue
			}

			byLocale[t.LangISO] = append(byLocale[t.LangISO], model.Translation{
				KeyID:      keyID,
				LocaleID:   locale.ID,
				Value:      t.Value,
				RenderHint: model.RenderHintPlain,
				UpdatedBy:  opts.Actor,
			})
		}
	}

	for _, locale := range locales {
		batch := byLocale[locale.Code]
		if err := s.translations.UpsertBatch(ctx, tx, batch); err != nil {
			return err
		}
		result.TranslationsWritten += len(batch)
	}

	return nil
}

// loadPresence reads each locale's exported JSON and records which keys it
// contains.
//
// A missing export file is FATAL, not a warning. Continuing without it would
// silently treat every key in that locale as untranslated and delete the whole
// column on the next export.
func (s *Service) loadPresence(
	root string, locales []model.Locale, result *Result,
) (map[string]map[string]bool, error) {
	present := make(map[string]map[string]bool, len(locales))

	for _, locale := range locales {
		path := filepath.Join(root, locale.FlutterDir, locale.FlutterDir+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf(
				"presence oracle: cannot read %s: %w (without it every %s value would be treated as untranslated)",
				path, err, locale.Code)
		}

		file, err := parse.JSONBytes(data)
		if err != nil {
			return nil, fmt.Errorf("presence oracle: parse %s: %w", path, err)
		}

		keys := make(map[string]bool, file.Len())
		for _, e := range file.Entries {
			keys[e.Key] = true
		}
		present[locale.Code] = keys

		for key, values := range file.Duplicates() {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s: export contains key %q %d times; presence is unaffected but the source should be cleaned",
				locale.Code, key, len(values)))
		}
	}

	return present, nil
}

// mapPlatforms converts Lokalise's platform names to ours. 'web' is Lokalise's
// name for what this project calls flutter.
func mapPlatforms(in []string) []model.Platform {
	seen := make(map[model.Platform]bool, len(in))
	for _, p := range in {
		switch p {
		case "web":
			seen[model.PlatformFlutter] = true
		case "android":
			seen[model.PlatformAndroid] = true
		case "ios":
			seen[model.PlatformIOS] = true
		}
	}

	out := make([]model.Platform, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	if len(out) == 0 {
		// keys_platforms_check forbids an empty array, and a key on no platform
		// would be exported nowhere. Flutter is the broadest set.
		out = append(out, model.PlatformFlutter)
	}
	return out
}

func hasLocale(locales []model.Locale, code string) bool {
	_, ok := localeByCode(locales, code)
	return ok
}

func localeByCode(locales []model.Locale, code string) (model.Locale, bool) {
	for _, l := range locales {
		if l.Code == code {
			return l, true
		}
	}
	return model.Locale{}, false
}
