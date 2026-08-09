// Package model holds the domain types. It imports the stdlib only — never
// another package of this service, and never a third-party dependency.
package model

import "time"

// Platform identifies which app consumes a key.
type Platform string

const (
	PlatformFlutter Platform = "flutter"
	PlatformAndroid Platform = "android"
	PlatformIOS     Platform = "ios"
)

// KeyStatus mirrors the keys.status CHECK constraint.
type KeyStatus string

const (
	KeyStatusActive  KeyStatus = "active"
	KeyStatusDeleted KeyStatus = "deleted"
	KeyStatusDraft   KeyStatus = "draft"
)

// RenderHint mirrors the translations.render_hint CHECK constraint.
type RenderHint string

const (
	RenderHintPlain RenderHint = "plain"
	RenderHintCDATA RenderHint = "cdata"
)

// Project is the root of the ownership tree. Every key, locale, branch and
// release belongs to exactly one. Codes appear in URLs, so they are slugs.
type Project struct {
	ID                int16
	Code              string
	Name              string
	Status            string
	LokaliseProjectID string // empty when the project has no Lokalise source
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Locale is one language/region, and owns its export directory naming so the
// serializers stay table-driven.
type Locale struct {
	ID               int16
	ProjectID        int16
	Code             string
	FlutterDir       string
	AndroidValuesDir string
	IOSLproj         string
	SortOrder        int16
	Status           string
}

// Key is one translatable string identifier.
type Key struct {
	ID          int64
	Name        string
	Description string
	Platforms   []Platform
	// AndroidName and IOSName are nil when derived from Name. A non-nil value
	// means the derivation was deliberately overridden — a distinction that
	// cannot be recovered if the derived value is materialised instead.
	AndroidName   *string
	IOSName       *string
	Status        KeyStatus
	Version       int
	SortIndex     int64
	LokaliseKeyID *int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Translation is one value for one (key, locale).
//
// There is no "empty" sentinel: the ABSENCE of a Translation means
// untranslated, and Value == "" means deliberately blank. Those are different
// facts and the repository never collapses them.
type Translation struct {
	KeyID      int64
	LocaleID   int16
	Value      string
	RenderHint RenderHint
	Version    int
	UpdatedBy  string
	UpdatedAt  time.Time
}

// HistorySource mirrors the *_history.source CHECK constraint.
type HistorySource string

const (
	SourceUI       HistorySource = "ui"
	SourceMerge    HistorySource = "merge"
	SourceImport   HistorySource = "import"
	SourceRollback HistorySource = "rollback"
	SourceAPI      HistorySource = "api"
)
