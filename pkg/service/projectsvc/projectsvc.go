// Package projectsvc owns the projects table's one write path that is not
// scoped to a project: minting a new one.
//
// Every other service in this codebase operates inside a project that
// already exists. This one has to create the thing everything else hangs
// off, and it has to leave the result usable: a project whose creator holds
// no role on it is a project nobody can configure, so Create writes the
// project row and the creator's admin grant in the same transaction — see
// .db/V1.13__scope_identity.sql for why that grant lives in a separate table
// from the project itself.
package projectsvc

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// ErrBadRequest marks a caller error, so the handler maps it to 400 rather
// than 500. A malformed code or an unknown status is a typo, not a fault.
var ErrBadRequest = errors.New("bad request")

// codePattern constrains what may appear in a URL path. The database has the
// same CHECK; this exists so a caller gets 400 with a message instead of a
// driver error.
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// localeCodePattern is BCP-47-ish: "en" or "en-SG". The database has no CHECK
// on locales.code (unlike projects.code), so this is the only validation a
// malformed locale code gets before it would otherwise reach the driver as
// an opaque error.
var localeCodePattern = regexp.MustCompile(`^[a-z]{2}(-[A-Z]{2})?$`)

const maxNameLen = 128

// NewProject is what Create needs to mint a project.
type NewProject struct {
	Code              string
	Name              string
	LokaliseProjectID string
}

// ProjectPatch is what Update may change. All three fields are written on
// every call — there is no partial-update sentinel here, matching
// ProjectRepository.Update, which takes the same three as plain strings.
type ProjectPatch struct {
	Name              string
	Status            string
	LokaliseProjectID string
}

// NewLocale is what AddLocale needs to add a locale to a project.
type NewLocale struct {
	Code             string
	FlutterDir       string
	AndroidValuesDir string
	IOSLproj         string
	SortOrder        int16
}

// LocalePatch is what UpdateLocale may change. Every field but Code is
// written on every call, matching ProjectPatch and LocaleRepository.Update:
// there is no partial-update sentinel here.
//
// Code is deliberately ABSENT — see LocaleRepository.Update. It is the
// identifier callers address the locale by, and renaming it would silently
// orphan every translation and history row that references it.
type LocalePatch struct {
	FlutterDir       string
	AndroidValuesDir string
	IOSLproj         string
	SortOrder        int16
	Status           string
}

// Service performs project writes. It owns the transaction boundary; no
// repository it calls opens one.
type Service struct {
	tx       database.Transactional
	projects repository.ProjectRepository
	roles    repository.UserProjectRoleRepository
	locales  repository.LocaleRepository
}

func ProvideService(
	tx database.Transactional,
	projects repository.ProjectRepository,
	roles repository.UserProjectRoleRepository,
	locales repository.LocaleRepository,
) *Service {
	return &Service{tx: tx, projects: projects, roles: roles, locales: locales}
}

// List returns every project the caller may reach.
func (s *Service) List(ctx context.Context, includeArchived bool) ([]model.Project, error) {
	return s.projects.List(ctx, nil, includeArchived)
}

// Create validates, then writes the project and its creator's admin grant in
// ONE transaction. A project whose creator cannot administer it is a project
// nobody can configure, and the two writes must not be able to diverge.
//
// repository.ErrProjectCodeTaken passes through untouched — the handler maps
// it to 409, and wrapping it further here would only make errors.Is do more
// work for no benefit.
func (s *Service) Create(ctx context.Context, actor string, in NewProject) (model.Project, error) {
	var created model.Project

	code, err := validateCode(in.Code)
	if err != nil {
		return created, err
	}
	name, err := validateName(in.Name)
	if err != nil {
		return created, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		// user_project_roles.granted_by is NOT NULL, and a blank grantor
		// answers nothing about who minted the project.
		return created, fmt.Errorf("%w: actor is required", ErrBadRequest)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		p, err := s.projects.Create(ctx, tx, model.Project{
			Code:              code,
			Name:              name,
			LokaliseProjectID: in.LokaliseProjectID,
		})
		if err != nil {
			return err
		}
		created = p

		return s.roles.Grant(ctx, tx, actor, p.ID, repository.RoleAdmin, actor)
	})
	if err != nil {
		return model.Project{}, err
	}

	log.Infow(ctx, "project created", "project_id", created.ID, "code", created.Code, "actor", actor)
	return created, nil
}

// Update changes a project's name, status or Lokalise linkage.
//
// The lookup and the write share a transaction so a project archived by
// another request between the two cannot silently reappear active under a
// caller who never asked for that.
func (s *Service) Update(ctx context.Context, code string, in ProjectPatch) (model.Project, error) {
	var updated model.Project

	code = strings.TrimSpace(code)
	if code == "" {
		return updated, fmt.Errorf("%w: project code is required", ErrBadRequest)
	}
	name, err := validateName(in.Name)
	if err != nil {
		return updated, err
	}
	switch in.Status {
	case repository.ProjectActive, repository.ProjectArchived:
	default:
		return updated, fmt.Errorf("%w: status must be %s or %s, got %q",
			ErrBadRequest, repository.ProjectActive, repository.ProjectArchived, in.Status)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		p, err := s.projects.ByCode(ctx, tx, code)
		if err != nil {
			return err
		}

		u, err := s.projects.Update(ctx, tx, p.ID, name, in.Status, in.LokaliseProjectID)
		if err != nil {
			return err
		}
		updated = u
		return nil
	})
	if err != nil {
		return model.Project{}, err
	}

	log.Infow(ctx, "project updated", "project_id", updated.ID, "code", updated.Code, "status", updated.Status)
	return updated, nil
}

// AddLocale adds a locale to a project.
//
// Adding a locale writes no translation rows — absent means untranslated, so
// a new locale starts empty and fills in as translators work. That is what
// makes arbitrary locale counts cheap, and it is why there is no bulk-seed
// path here: one locale is one INSERT, same as any other.
//
// repository.ErrLocaleCodeTaken and ErrLocaleDirectoryTaken pass through
// untouched, exactly like ErrProjectCodeTaken on Create above — the handler
// maps both to 409 with errors.Is, and wrapping them further here would only
// make that matching do more work for no benefit.
func (s *Service) AddLocale(ctx context.Context, projectCode string, in NewLocale) (model.Locale, error) {
	var created model.Locale

	code, err := validateLocaleCode(in.Code)
	if err != nil {
		return created, err
	}
	flutterDir, err := validateDirName("flutter_dir", in.FlutterDir)
	if err != nil {
		return created, err
	}
	androidDir, err := validateDirName("android_values_dir", in.AndroidValuesDir)
	if err != nil {
		return created, err
	}
	iosDir, err := validateDirName("ios_lproj", in.IOSLproj)
	if err != nil {
		return created, err
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		p, err := s.projects.ByCode(ctx, tx, projectCode)
		if err != nil {
			return err
		}

		l, err := s.locales.Create(ctx, tx, model.Locale{
			ProjectID:        p.ID,
			Code:             code,
			FlutterDir:       flutterDir,
			AndroidValuesDir: androidDir,
			IOSLproj:         iosDir,
			SortOrder:        in.SortOrder,
		})
		if err != nil {
			return err
		}
		created = l
		return nil
	})
	if err != nil {
		return model.Locale{}, err
	}

	log.Infow(ctx, "locale added", "project_code", projectCode, "locale_code", created.Code)
	return created, nil
}

// UpdateLocale changes a locale's export directories, sort order or status.
//
// The code is not a parameter of LocalePatch and cannot be changed through
// this method — see LocalePatch's doc comment. The lookup and the write
// share a transaction for the same reason Update does above: a project
// archived, or a locale archived by another request, between the two must
// not be able to reappear silently under a caller who never asked for that.
func (s *Service) UpdateLocale(ctx context.Context, projectCode, code string, in LocalePatch) (model.Locale, error) {
	var updated model.Locale

	code = strings.TrimSpace(code)
	if code == "" {
		return updated, fmt.Errorf("%w: locale code is required", ErrBadRequest)
	}
	flutterDir, err := validateDirName("flutter_dir", in.FlutterDir)
	if err != nil {
		return updated, err
	}
	androidDir, err := validateDirName("android_values_dir", in.AndroidValuesDir)
	if err != nil {
		return updated, err
	}
	iosDir, err := validateDirName("ios_lproj", in.IOSLproj)
	if err != nil {
		return updated, err
	}
	switch in.Status {
	case repository.LocaleActive, repository.LocaleArchived:
	default:
		return updated, fmt.Errorf("%w: status must be %s or %s, got %q",
			ErrBadRequest, repository.LocaleActive, repository.LocaleArchived, in.Status)
	}

	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		p, err := s.projects.ByCode(ctx, tx, projectCode)
		if err != nil {
			return err
		}

		u, err := s.locales.Update(ctx, tx, p.ID, code, model.Locale{
			FlutterDir:       flutterDir,
			AndroidValuesDir: androidDir,
			IOSLproj:         iosDir,
			SortOrder:        in.SortOrder,
			Status:           in.Status,
		})
		if err != nil {
			return err
		}
		updated = u
		return nil
	})
	if err != nil {
		return model.Locale{}, err
	}

	log.Infow(ctx, "locale updated", "project_code", projectCode,
		"locale_code", updated.Code, "status", updated.Status)
	return updated, nil
}

// validateLocaleCode enforces the BCP-47-ish shape the database has no CHECK
// for, so a malformed code reads as a 400 with a message instead of a
// confusing downstream failure (or, worse, silent acceptance).
func validateLocaleCode(code string) (string, error) {
	if !localeCodePattern.MatchString(code) {
		return "", fmt.Errorf("%w: locale code must match %s, got %q",
			ErrBadRequest, localeCodePattern.String(), code)
	}
	return code, nil
}

// validateDirName requires a non-empty export directory name. All three
// (flutter_dir, android_values_dir, ios_lproj) are required on every write —
// there is no NULL/omitted state for them, unlike a key's android_name.
func validateDirName(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: %s is required", ErrBadRequest, field)
	}
	return trimmed, nil
}

// validateCode enforces what the database's CHECK also enforces, so a
// mistyped code reads as a 400 with a message rather than a driver error.
func validateCode(code string) (string, error) {
	if !codePattern.MatchString(code) {
		return "", fmt.Errorf("%w: code must match %s, got %q", ErrBadRequest, codePattern.String(), code)
	}
	return code, nil
}

func validateName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name is required", ErrBadRequest)
	}
	if len(name) > maxNameLen {
		return "", fmt.Errorf("%w: name must be at most %d characters, got %d",
			ErrBadRequest, maxNameLen, len(name))
	}
	return trimmed, nil
}
