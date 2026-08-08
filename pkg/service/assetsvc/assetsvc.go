// Package assetsvc owns context screenshots: the presigned-upload handshake,
// the post-upload verification, and the key↔asset links.
//
// The flow is three steps and the middle one does not involve this service at
// all:
//
//  1. Presign — the browser declares what it is about to upload; we validate
//     the declaration, bind it into a signed POST policy, and hand back a form.
//  2. The browser POSTs the bytes straight to S3. Nothing passes through here,
//     which is the whole point: a 10MB screenshot never occupies a request
//     slot, a request timeout or a pod's memory.
//  3. Confirm — we HEAD the object ourselves and only then insert the row.
//
// Step 3 exists because the browser's "it worked" is not evidence. Trusting it
// produces `assets` rows pointing at objects that were never uploaded, and the
// failure is invisible until a translator opens a screenshot months later and
// gets a 404 from a URL we signed.
//
// SECURITY: these are screenshots of a fintech app and routinely contain
// customer names, balances, card numbers and transaction history. The bucket is
// private; reads are short-lived presigned GETs, and every one of them is
// audited before it is issued.
package assetsvc

import (
	"context"
	cryptosha "crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jinzhu/gorm"
	"github.com/minio/minio-go"

	"github.com/yougroupteam/u-common-components/database"
	storage "github.com/yougroupteam/u-common-components/storage/v4"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

var log = ulog.GetLogger("u-l10n")

// ObjectStore is the narrowest view of storage/v4 this service needs.
//
// Declared here rather than depending on storage.Storage for the same reason
// route.Pinger exists: there is no local S3 or MinIO anywhere in this tree, and
// storage/v4 hardcodes TLS with no path-style option, so the real client cannot
// be pointed at a test double of a bucket. A three-method interface can be, and
// the refusal paths below are the ones that most need testing.
type ObjectStore interface {
	Attributes(ctx context.Context, bucketName, fileName string) (storage.ObjectAttributes, error)
	SignURL(ctx context.Context, bucketName, method, filename, key string, opts storage.Options) (
		urlStr string, form map[string]string, err error)
	// Read fetches the whole object. Used by Confirm to verify the bytes hash
	// to the name they were stored under; bounded in practice by the size
	// checks that run before any Read is issued.
	Read(ctx context.Context, bucketName, fileName string) ([]byte, error)
}

// Limits mirroring the CHECK constraints in .db/V1.04__assets.sql.
//
// Duplicated deliberately: the database is the last line of defence and must
// keep its constraints, but a 10MB upload rejected only at INSERT time has
// already crossed the network twice. These reject it before a URL is even
// signed.
const (
	// MaxBytes matches assets_size_check.
	MaxBytes = 10 * 1024 * 1024

	// SHA256HexLen matches assets_sha256_format_check.
	SHA256HexLen = 64

	// maxFilenameLen bounds the cosmetic filename. It travels as an S3 user
	// metadata header, and S3 caps total metadata at 2KB.
	maxFilenameLen = 200
)

// storableContentTypes matches assets_content_type_check — what the database
// will accept in a row.
var storableContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// presignExtensions maps a content type to the filename extension that makes
// storage/v4 pin that content type in the POST policy.
//
// This indirection is not cosmetic. storage/v4's POST branch derives the
// policy's Content-Type condition from the *filename argument's extension* via
// its own determineContentType, and ignores Options.ContentType entirely — the
// same field the PUT branch also ignores. Passing the caller's filename
// straight through would therefore let "shot.jpg" declared as image/png sign a
// policy pinning image/jpeg, and the row would disagree with the object. The
// extension is derived from the validated content type instead, and the
// caller's filename is carried separately as metadata.
//
// image/webp is absent because storage/v4's determineContentType does not know
// it and returns "", which makes minio's SetContentType fail and takes the
// whole signing call down. Rather than emit a 500 for a type the schema
// permits, presign rejects webp with an explanatory 400. Widening this map is
// a one-line change once storage/v4 learns the type.
var presignExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
}

// Metadata keys that carry the declaration from presign to confirm.
//
// These become x-amz-meta-* form fields with `eq` conditions in the signed
// policy, so S3 rejects an upload that does not carry exactly the values we
// signed. That is what makes confirm's comparison meaningful: the declaration
// is fixed at presign time and the client cannot revise it afterwards to match
// whatever it actually uploaded.
const (
	metaDeclaredBytes       = "declared-bytes"
	metaDeclaredContentType = "declared-content-type"
	metaDeclaredFilename    = "declared-filename"
)

var (
	// ErrBadRequest marks a caller error so the handler maps it to 400 rather
	// than 500.
	ErrBadRequest = errors.New("bad request")

	// ErrUploadNotFound means the client confirmed an upload that is not in the
	// bucket. This is the refusal that matters: without it, a client that
	// simply skipped step 2 would still get a row.
	ErrUploadNotFound = errors.New("no uploaded object for that hash")

	// ErrUploadMismatch means the object exists but is not what was declared —
	// a different size or a different content type.
	ErrUploadMismatch = errors.New("uploaded object does not match what was declared")

	// ErrUploadUnverifiable means the object carries no declaration, so there
	// is nothing to check it against. An object in our own key space that did
	// not come from our own presign is not something to admit on trust.
	ErrUploadUnverifiable = errors.New("uploaded object carries no declaration")
)

// Service performs the asset flow. It owns the transaction boundary for every
// write, and no repository it calls opens one.
type Service struct {
	tx     database.Transactional
	store  ObjectStore
	assets repository.AssetRepository
	audit  repository.AuditRepository
}

// ProvideService takes the concrete storage.Storage because that is the type
// Wire has a provider for; the field it lands in is the narrow interface above,
// which is what the rest of the package — and the tests — depend on.
func ProvideService(
	tx database.Transactional,
	store storage.Storage,
	assets repository.AssetRepository,
	audit repository.AuditRepository,
) *Service {
	return &Service{tx: tx, store: store, assets: assets, audit: audit}
}

// PresignRequest is a client's declaration of what it intends to upload.
type PresignRequest struct {
	Filename    string
	ContentType string
	Bytes       int
	SHA256      string
}

// Upload is everything a browser needs to POST the bytes to S3.
type Upload struct {
	URL    string
	Fields map[string]string
	S3Key  string
}

// PresignResult is either an existing asset or an upload form, never both.
type PresignResult struct {
	// Asset is set when these exact bytes are already stored. Upload is then
	// nil and there is nothing to transfer.
	Asset *repository.Asset
	// Upload is set when the bytes are new.
	Upload *Upload
}

// Presign validates a declaration and returns either the existing asset or an
// upload form.
//
// Dedupe happens BEFORE any call to S3. Content addressing means identical
// bytes always produce the same row, so re-uploading a screenshot someone else
// already attached transfers nothing and signs nothing — the request never
// leaves this process.
func (s *Service) Presign(ctx context.Context, req PresignRequest, actor string) (PresignResult, error) {
	var result PresignResult

	sum, err := normaliseSHA256(req.SHA256)
	if err != nil {
		return result, err
	}
	if req.Bytes <= 0 || req.Bytes > MaxBytes {
		return result, fmt.Errorf("%w: bytes must be between 1 and %d, got %d",
			ErrBadRequest, MaxBytes, req.Bytes)
	}
	filename, err := sanitiseFilename(req.Filename)
	if err != nil {
		return result, err
	}
	ext, ok := presignExtensions[req.ContentType]
	if !ok {
		if storableContentTypes[req.ContentType] {
			return result, fmt.Errorf(
				"%w: %s cannot be presigned by this service — the upload policy cannot pin that content type",
				ErrBadRequest, req.ContentType)
		}
		return result, fmt.Errorf("%w: content_type must be image/png or image/jpeg, got %q",
			ErrBadRequest, req.ContentType)
	}

	existing, err := s.assets.BySHA256(ctx, nil, sum)
	switch {
	case err == nil:
		log.Infow(ctx, "asset already stored, no upload needed",
			"asset_id", existing.ID, "sha256", sum, "actor", actor)
		result.Asset = &existing
		return result, nil
	case errors.Is(err, repository.ErrNotFound):
		// New bytes. Fall through and sign.
	default:
		return result, err
	}

	key := s3Key(sum)

	// http.MethodPost, NOT http.MethodPut.
	//
	// The PUT branch of SignURL produces a bare presigned URL with no
	// conditions at all: it ignores Options.ContentType, so the browser may
	// upload anything of any size to the key we signed. Every validation above
	// would then be advisory. The POST branch signs a policy whose conditions
	// S3 itself enforces, which is the only way a declaration made here
	// constrains what actually lands in the bucket.
	//
	// The bucket is left empty so storage/v4 uses its configured default.
	url, fields, err := s.store.SignURL(ctx, "", http.MethodPost, sum+"."+ext, key, storage.Options{
		Metadata: map[string]string{
			metaDeclaredBytes:       strconv.Itoa(req.Bytes),
			metaDeclaredContentType: req.ContentType,
			metaDeclaredFilename:    filename,
		},
	})
	if err != nil {
		return result, fmt.Errorf("sign upload policy: %w", err)
	}

	// The URL and the form fields are credentials to write into our bucket.
	// Neither is logged, here or anywhere else.
	log.Infow(ctx, "issued upload policy",
		"sha256", sum, "bytes", req.Bytes, "content_type", req.ContentType, "actor", actor)

	result.Upload = &Upload{URL: url, Fields: fields, S3Key: key}
	return result, nil
}

// Confirm verifies an upload actually landed and records the asset.
//
// The client sends only the hash. Everything else is read back from the object
// itself, so there is no second declaration to disagree with the first.
func (s *Service) Confirm(ctx context.Context, sha256 string, actor, requestID string) (repository.Asset, error) {
	var asset repository.Asset

	sum, err := normaliseSHA256(sha256)
	if err != nil {
		return asset, err
	}

	// Idempotent: confirming twice, or confirming bytes someone else already
	// confirmed, returns the same row. A retried request after a dropped
	// response must not be an error.
	if existing, err := s.assets.BySHA256(ctx, nil, sum); err == nil {
		return existing, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return asset, err
	}

	key := s3Key(sum)

	// A real HEAD against the bucket. This is the load-bearing line of the
	// whole package.
	attrs, err := s.store.Attributes(ctx, "", key)
	if err != nil {
		// Classify: a missing object is the caller's problem, anything else is
		// ours. Conflating them either sends an on-call engineer after a
		// non-fault, or buries a genuine S3 outage in a 4xx.
		if code := minio.ToErrorResponse(err).Code; code == "NoSuchKey" || code == "NoSuchBucket" {
			log.Infow(ctx, "confirm rejected: no object for hash", "sha256", sum, "actor", actor)
			return asset, fmt.Errorf("%w (sha256 %s)", ErrUploadNotFound, sum)
		}
		return asset, fmt.Errorf("stat uploaded object: %w", err)
	}

	declaredBytes, declaredType, declaredName, err := readDeclaration(attrs)
	if err != nil {
		log.Infow(ctx, "confirm rejected: object carries no declaration", "sha256", sum, "actor", actor)
		return asset, err
	}

	// Declared versus actual. The policy pins the content type, but it cannot
	// pin the size — storage/v4 sets no content-length-range condition — so a
	// client is free to declare 1MB, take the URL and push 50MB. This is where
	// that is caught.
	if attrs.FileSize != int64(declaredBytes) {
		log.Infow(ctx, "confirm rejected: size mismatch", "sha256", sum,
			"declared", declaredBytes, "actual", attrs.FileSize, "actor", actor)
		return asset, fmt.Errorf("%w: declared %d bytes, object is %d",
			ErrUploadMismatch, declaredBytes, attrs.FileSize)
	}
	if mediaType(attrs.ContentType) != declaredType {
		log.Infow(ctx, "confirm rejected: content type mismatch", "sha256", sum,
			"declared", declaredType, "actual", attrs.ContentType, "actor", actor)
		return asset, fmt.Errorf("%w: declared %s, object is %s",
			ErrUploadMismatch, declaredType, attrs.ContentType)
	}

	// The declaration is only as trustworthy as the build that signed it. Check
	// the actual object against the hard limits too, so an object signed by an
	// older or laxer presign cannot become a row that violates today's rules.
	if attrs.FileSize <= 0 || attrs.FileSize > MaxBytes {
		return asset, fmt.Errorf("%w: object is %d bytes, limit is %d",
			ErrUploadMismatch, attrs.FileSize, MaxBytes)
	}
	if !storableContentTypes[declaredType] {
		return asset, fmt.Errorf("%w: %s is not a permitted content type",
			ErrUploadMismatch, declaredType)
	}

	// The bytes must hash to the name they claim. Everything above checked the
	// object against its DECLARATION; nothing yet has checked it against its
	// ADDRESS, and the address is the whole design — "the name IS the content"
	// is what lets dedupe return an existing row instead of an upload.
	//
	// Without this read, a client whose hash is wrong — buggy or malicious —
	// creates a row whose sha256 does not match its bytes, and dedupe then
	// PROPAGATES the corruption: every future upload of the genuine bytes is
	// told "already stored" and attaches the wrong image, permanently and
	// silently. One bounded GET per new asset is the entire price of making
	// that impossible, and it runs last so the cheap refusals above never pay
	// it.
	body, err := s.store.Read(ctx, "", key)
	if err != nil {
		// Reading what HEAD just saw failing is an infrastructure fault, not a
		// caller error.
		return asset, fmt.Errorf("read uploaded object for verification: %w", err)
	}
	if actual := hashHex(body); actual != sum {
		log.Infow(ctx, "confirm rejected: content does not hash to its name",
			"claimed", sum, "actual", actual, "actor", actor)
		return asset, fmt.Errorf("%w: object hashes to %s, not the claimed %s",
			ErrUploadMismatch, actual, sum)
	}

	// Width and height stay NULL. Decoding the image to fill them would mean
	// pulling 10MB of customer PII through this process for two integers no
	// part of the system reads.
	err = s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		created, err := s.assets.Create(ctx, tx, repository.Asset{
			S3Key:       key,
			SHA256:      sum,
			Filename:    declaredName,
			ContentType: declaredType,
			Bytes:       int(attrs.FileSize),
			UploadedBy:  actor,
		})
		if err != nil {
			return err
		}
		asset = created

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionAssetCreate,
			Target:    fmt.Sprintf("asset:%d", created.ID),
			Metadata:  map[string]any{"sha256": sum, "bytes": created.Bytes},
			RequestID: requestID,
		})
	})
	if err != nil {
		return repository.Asset{}, err
	}

	log.Infow(ctx, "asset confirmed", "asset_id", asset.ID, "sha256", sum,
		"bytes", asset.Bytes, "actor", actor)
	return asset, nil
}

// SignedURL issues a short-lived presigned GET and audits the view.
//
// The audit row is written FIRST and its failure fails the request. That is a
// deliberate departure from the best-effort audit in APITokenRepository.
// Authenticate: losing a last-used timestamp is not worth a 500, but issuing a
// credential to customer PII with no record of who received it is exactly the
// question this trail exists to answer.
func (s *Service) SignedURL(ctx context.Context, assetID int64, actor, requestID string) (string, error) {
	asset, err := s.assets.ByID(ctx, nil, assetID)
	if err != nil {
		return "", err
	}

	err = s.audit.Record(ctx, nil, repository.AuditEvent{
		Actor:     actor,
		Action:    repository.ActionAssetView,
		Target:    fmt.Sprintf("asset:%d", asset.ID),
		Metadata:  map[string]any{"sha256": asset.SHA256},
		RequestID: requestID,
	})
	if err != nil {
		return "", err
	}

	url, _, err := s.store.SignURL(ctx, "", http.MethodGet, "", asset.S3Key, storage.Options{})
	if err != nil {
		return "", fmt.Errorf("sign asset url: %w", err)
	}

	// The asset id, never the URL: a presigned GET is a bearer credential for
	// a screenshot full of customer data, and a log line is not a place to keep
	// one.
	log.Infow(ctx, "issued asset url", "asset_id", asset.ID, "actor", actor)
	return url, nil
}

// Attach links an asset to a key.
func (s *Service) Attach(ctx context.Context, keyID, assetID int64, note, actor, requestID string) error {
	note = strings.TrimSpace(note)
	if len(note) > 1000 {
		return fmt.Errorf("%w: note must be at most 1000 characters", ErrBadRequest)
	}

	return s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		// Both existence checks happen inside the transaction that acts on
		// them. Checking outside would be a decoration: the row can go away
		// between the check and the insert.
		exists, err := s.assets.KeyExists(ctx, tx, keyID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("key %d: %w", keyID, repository.ErrNotFound)
		}
		if _, err := s.assets.ByID(ctx, tx, assetID); err != nil {
			return err
		}

		if err := s.assets.Attach(ctx, tx, keyID, assetID, note, actor); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionAssetAttach,
			Target:    fmt.Sprintf("asset:%d", assetID),
			Metadata:  map[string]any{"key_id": keyID, "note": note},
			RequestID: requestID,
		})
	})
}

// Detach removes the link between a key and an asset. The asset row and the
// object are left alone: another key may still reference them, and the bytes
// are content-addressed and therefore shared by construction.
func (s *Service) Detach(ctx context.Context, keyID, assetID int64, actor, requestID string) error {
	return s.tx.WithTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.assets.Detach(ctx, tx, keyID, assetID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, repository.AuditEvent{
			Actor:     actor,
			Action:    repository.ActionAssetDetach,
			Target:    fmt.Sprintf("asset:%d", assetID),
			Metadata:  map[string]any{"key_id": keyID},
			RequestID: requestID,
		})
	})
}

// s3Key returns the content-addressed object key.
//
// The two-byte fan-out directories exist because S3 partitions on key prefix:
// a flat screenshots/<sha> namespace concentrates every write on one partition.
//
// The key carries no file extension, unlike the illustrative comment on
// assets.s3_key. Confirm receives only the hash, so an extension would have to
// be guessed or searched for before the object could be HEADed, and it encodes
// nothing the object's own Content-Type header and assets.content_type do not
// already hold.
// hashHex returns the lowercase hex SHA-256 of b — the same canonical form
// assets_sha256_format_check enforces, so a comparison against a stored sum
// can never miss on encoding.
func hashHex(b []byte) string {
	sum := cryptosha.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func s3Key(sha256 string) string {
	return fmt.Sprintf("screenshots/%s/%s/%s", sha256[0:2], sha256[2:4], sha256)
}

// mediaType strips any parameters and case from a Content-Type header.
//
// "image/png; charset=binary" and "IMAGE/PNG" are the same media type, and a
// comparison that treats them as different would refuse a perfectly good upload
// on the strength of a header some proxy or SDK decorated.
func mediaType(header string) string {
	if i := strings.IndexByte(header, ';'); i >= 0 {
		header = header[:i]
	}
	return strings.ToLower(strings.TrimSpace(header))
}

// normaliseSHA256 enforces assets_sha256_format_check before the value can
// reach a key, a query or the database.
//
// Lowercase hex is the one canonical encoding; accepting uppercase would let
// the same bytes produce two keys, two rows, and defeat dedupe entirely.
func normaliseSHA256(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != SHA256HexLen {
		return "", fmt.Errorf("%w: sha256 must be %d hex characters, got %d",
			ErrBadRequest, SHA256HexLen, len(s))
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("%w: sha256 must be lowercase hex", ErrBadRequest)
		}
	}
	return s, nil
}

// sanitiseFilename reduces a filename to something safe to carry in an HTTP
// header and to display in a portal.
//
// It rewrites rather than rejects: the filename is cosmetic, and a translator
// whose screenshot is called "Écran — 3.png" should not have an upload refused
// over a character the row never depends on. Directory components are dropped,
// because a filename is not a path and treating one as a path is how traversal
// bugs start.
func sanitiseFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			// Anything outside printable ASCII would be mangled or rejected by
			// S3's metadata header encoding.
			b.WriteByte('_')
		}
	}
	cleaned := strings.Trim(b.String(), "._ ")

	if cleaned == "" {
		return "", fmt.Errorf("%w: filename is required", ErrBadRequest)
	}
	if len(cleaned) > maxFilenameLen {
		cleaned = cleaned[:maxFilenameLen]
	}
	return cleaned, nil
}

// readDeclaration recovers the presign-time declaration from the object's user
// metadata.
//
// Lookup is case-insensitive over the whole header name. Go canonicalises
// response headers to X-Amz-Meta-Declared-Bytes, but the exact casing is a
// property of whatever HTTP stack parsed the response, and this must not turn
// into a silent "no declaration" when it changes.
func readDeclaration(attrs storage.ObjectAttributes) (bytes int, contentType, filename string, err error) {
	get := func(suffix string) string {
		want := "x-amz-meta-" + suffix
		for name, values := range attrs.Metadata {
			if strings.EqualFold(name, want) && len(values) > 0 {
				return strings.TrimSpace(values[0])
			}
		}
		return ""
	}

	rawBytes := get(metaDeclaredBytes)
	contentType = strings.ToLower(get(metaDeclaredContentType))
	filename = get(metaDeclaredFilename)

	if rawBytes == "" || contentType == "" || filename == "" {
		return 0, "", "", ErrUploadUnverifiable
	}

	bytes, err = strconv.Atoi(rawBytes)
	if err != nil || bytes <= 0 {
		return 0, "", "", fmt.Errorf("%w: declared size %q is not a positive integer",
			ErrUploadUnverifiable, rawBytes)
	}

	return bytes, contentType, filename, nil
}
