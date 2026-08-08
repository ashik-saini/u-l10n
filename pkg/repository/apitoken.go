package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
)

// TokenPrefix marks u-l10n tokens so a leaked credential is identifiable at a
// glance — in a log, a CI variable, or a secret scanner.
const TokenPrefix = "ul10n_"

// APIToken is a script credential. The plaintext token is NEVER stored and
// never appears on this struct.
type APIToken struct {
	ID     int64
	Name   string
	Scope  string
	Prefix string
}

const (
	ScopeReadExport = "read_export"
	ScopeReadWrite  = "read_write"
)

// APITokenRepository authenticates and issues script tokens.
type APITokenRepository interface {
	// Authenticate resolves a plaintext token to its record.
	//
	// Lookup is BY HASH: the plaintext is hashed and the hash is the query key,
	// so the database never sees the secret and a query log cannot leak it.
	// That also means no timing-safe comparison is needed here — there is no
	// comparison, only an indexed lookup.
	Authenticate(ctx context.Context, tx *gorm.DB, plaintext string) (APIToken, error)

	// Create issues a new token and returns the plaintext ONCE. It is never
	// recoverable afterwards.
	Create(ctx context.Context, tx *gorm.DB, name, scope, createdBy string, expiresAt *time.Time) (plaintext string, t APIToken, err error)

	Revoke(ctx context.Context, tx *gorm.DB, name, revokedBy string) error
}

type apiTokenRepository struct{ base }

func ProvideAPITokenRepository(connector database.GORMConnector) APITokenRepository {
	return &apiTokenRepository{base{connector: connector}}
}

// hashToken returns the lowercase hex SHA-256 of a token. Lowercase hex because
// the api_tokens_sha256_format_check constraint enforces exactly that — one
// canonical encoding, or a lookup silently misses.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// authenticateSQL resolves a live token by hash.
//
// The predicates are the authorisation decision, expressed in SQL so no caller
// can forget one: not revoked, and not expired. NULL expires_at means no expiry.
const authenticateSQL = `
SELECT id, name, scope, token_prefix
  FROM api_tokens
 WHERE token_sha256 = $1
   AND revoked_at IS NULL
   AND (expires_at IS NULL OR expires_at > now())`

func (r *apiTokenRepository) Authenticate(ctx context.Context, tx *gorm.DB, plaintext string) (APIToken, error) {
	var t APIToken
	if plaintext == "" {
		return t, ErrNotFound
	}

	db := r.db(ctx, tx)
	row := db.Raw(authenticateSQL, hashToken(plaintext)).Row()

	switch err := row.Scan(&t.ID, &t.Name, &t.Scope, &t.Prefix); {
	case err == nil:
	case isNoRows(err):
		// Deliberately indistinguishable from a wrong token: revoked, expired
		// and never-existed all return the same thing, so probing cannot
		// enumerate valid names.
		return t, ErrNotFound
	default:
		return t, fmt.Errorf("authenticate token: %w", err)
	}

	// Best-effort audit of use. A failure here must not fail the request — the
	// caller is legitimately authenticated and losing a timestamp is not worth
	// a 500.
	if err := db.Exec(
		`UPDATE api_tokens SET last_used_at = now() WHERE id = ?`, t.ID).Error; err != nil {
		log.Errore(ctx, "failed to record token use", err, "token_id", t.ID)
	}

	return t, nil
}

func (r *apiTokenRepository) Create(
	ctx context.Context, tx *gorm.DB, name, scope, createdBy string, expiresAt *time.Time,
) (string, APIToken, error) {
	var t APIToken

	switch scope {
	case ScopeReadExport, ScopeReadWrite:
	default:
		return "", t, fmt.Errorf("invalid scope %q: want %s or %s",
			scope, ScopeReadExport, ScopeReadWrite)
	}

	// 32 bytes from crypto/rand. base64url so the token is copy-pasteable into
	// a shell variable or CI secret without quoting.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", t, fmt.Errorf("generate token: %w", err)
	}
	plaintext := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	// The stored prefix is for display only — enough to recognise a token in a
	// list, far too little to reconstruct it.
	prefix := plaintext[:len(TokenPrefix)+6]

	row := r.db(ctx, tx).Raw(`
		INSERT INTO api_tokens (name, token_sha256, token_prefix, scope, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, name, scope, token_prefix`,
		name, hashToken(plaintext), prefix, scope, createdBy, expiresAt).Row()

	if err := row.Scan(&t.ID, &t.Name, &t.Scope, &t.Prefix); err != nil {
		return "", t, fmt.Errorf("create token %q: %w", name, err)
	}
	return plaintext, t, nil
}

func (r *apiTokenRepository) Revoke(ctx context.Context, tx *gorm.DB, name, revokedBy string) error {
	res := r.db(ctx, tx).Exec(`
		UPDATE api_tokens SET revoked_at = now(), revoked_by = $1
		 WHERE name = $2 AND revoked_at IS NULL`, revokedBy, name)
	if res.Error != nil {
		return fmt.Errorf("revoke token %q: %w", name, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("token %q: %w (or already revoked)", name, ErrNotFound)
	}
	return nil
}
