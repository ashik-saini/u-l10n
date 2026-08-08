package assetsvc

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/jinzhu/gorm"
	"github.com/minio/minio-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"
	storage "github.com/yougroupteam/u-common-components/storage/v4"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// The tests below lean hardest on the REFUSALS. A confirm that succeeds when
// the object is absent, or when it is ten times the declared size, produces an
// assets row that looks perfectly healthy and only fails months later when a
// translator opens the screenshot. There is no local S3 or MinIO in this tree
// and storage/v4 hardcodes TLS with no path-style option, so the real client
// cannot be pointed anywhere; the fake below stands in for the bucket, the same
// way a test double stands in for route.Pinger.

const testSHA = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

// --- fakes -----------------------------------------------------------------

type storeCall struct {
	method string
	// filename is what storage/v4 derives the policy's Content-Type from.
	filename string
	key      string
	opts     storage.Options
}

type fakeStore struct {
	signCalls []storeCall
	statCalls []string

	signURL    string
	signFields map[string]string
	signErr    error

	attrs    storage.ObjectAttributes
	attrsErr error
}

func (f *fakeStore) SignURL(
	_ context.Context, _, method, filename, key string, opts storage.Options,
) (string, map[string]string, error) {
	f.signCalls = append(f.signCalls, storeCall{method: method, filename: filename, key: key, opts: opts})
	if f.signErr != nil {
		return "", nil, f.signErr
	}
	url := f.signURL
	if url == "" {
		url = "https://bucket.example/upload"
	}
	return url, f.signFields, nil
}

func (f *fakeStore) Attributes(_ context.Context, _, fileName string) (storage.ObjectAttributes, error) {
	f.statCalls = append(f.statCalls, fileName)
	return f.attrs, f.attrsErr
}

// link records an attach or detach the fake was asked to perform.
type link struct {
	keyID, assetID int64
	note, by       string
}

type fakeAssets struct {
	bySHA    map[string]repository.Asset
	byID     map[int64]repository.Asset
	keyIDs   map[int64]bool
	created  []repository.Asset
	attached []link
	detached []link

	detachErr error
	nextID    int64
}

func newFakeAssets() *fakeAssets {
	return &fakeAssets{
		bySHA:  map[string]repository.Asset{},
		byID:   map[int64]repository.Asset{},
		keyIDs: map[int64]bool{},
		nextID: 1,
	}
}

func (f *fakeAssets) BySHA256(_ context.Context, _ *gorm.DB, sha string) (repository.Asset, error) {
	if a, ok := f.bySHA[sha]; ok {
		return a, nil
	}
	return repository.Asset{}, repository.ErrNotFound
}

func (f *fakeAssets) ByID(_ context.Context, _ *gorm.DB, id int64) (repository.Asset, error) {
	if a, ok := f.byID[id]; ok {
		return a, nil
	}
	return repository.Asset{}, repository.ErrNotFound
}

func (f *fakeAssets) Create(_ context.Context, _ *gorm.DB, a repository.Asset) (repository.Asset, error) {
	a.ID = f.nextID
	f.nextID++
	f.created = append(f.created, a)
	f.bySHA[a.SHA256] = a
	f.byID[a.ID] = a
	return a, nil
}

func (f *fakeAssets) Attach(_ context.Context, _ *gorm.DB, keyID, assetID int64, note, by string) error {
	f.attached = append(f.attached, link{keyID: keyID, assetID: assetID, note: note, by: by})
	return nil
}

func (f *fakeAssets) Detach(_ context.Context, _ *gorm.DB, keyID, assetID int64) error {
	if f.detachErr != nil {
		return f.detachErr
	}
	f.detached = append(f.detached, link{keyID: keyID, assetID: assetID})
	return nil
}

func (f *fakeAssets) KeyExists(_ context.Context, _ *gorm.DB, keyID int64) (bool, error) {
	return f.keyIDs[keyID], nil
}

type fakeAudit struct {
	events []repository.AuditEvent
	err    error
}

func (f *fakeAudit) Record(_ context.Context, _ *gorm.DB, e repository.AuditEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}

// fakeTx runs the body directly. The real helper does not nest, and neither
// does this: there is exactly one transaction per service call, which is the
// property the fake needs to preserve.
type fakeTx struct{ calls int }

func (f *fakeTx) WithTransaction(ctx context.Context, fn database.TransactionFunc) error {
	f.calls++
	return fn(nil)
}

type harness struct {
	svc    *Service
	store  *fakeStore
	assets *fakeAssets
	audit  *fakeAudit
	tx     *fakeTx
}

func newHarness() *harness {
	h := &harness{
		store:  &fakeStore{},
		assets: newFakeAssets(),
		audit:  &fakeAudit{},
		tx:     &fakeTx{},
	}
	// Constructed directly rather than through ProvideService: the fake stands
	// in for the narrow ObjectStore, which is the whole reason that interface
	// exists.
	h.svc = &Service{tx: h.tx, store: h.store, assets: h.assets, audit: h.audit}
	return h
}

// declaredAttrs builds what a HEAD returns for an object our presign signed.
func declaredAttrs(size int64, contentType string, declaredBytes int, declaredType, declaredName string) storage.ObjectAttributes {
	return storage.ObjectAttributes{
		FileSize:    size,
		ContentType: contentType,
		Metadata: map[string][]string{
			"X-Amz-Meta-Declared-Bytes":        {strconv.Itoa(declaredBytes)},
			"X-Amz-Meta-Declared-Content-Type": {declaredType},
			"X-Amz-Meta-Declared-Filename":     {declaredName},
		},
	}
}

// --- presign ---------------------------------------------------------------

// TestPresignDedupeIssuesNoUploadURL is the point of content addressing. The
// same screenshot pasted into three keys must be transferred once, and the
// second and third requests must not even reach S3.
func TestPresignDedupeIssuesNoUploadURL(t *testing.T) {
	h := newHarness()
	h.assets.bySHA[testSHA] = repository.Asset{ID: 7, SHA256: testSHA, Filename: "cart.png"}

	result, err := h.svc.Presign(context.Background(), PresignRequest{
		Filename: "cart.png", ContentType: "image/png", Bytes: 4096, SHA256: testSHA,
	}, "token:ci")

	require.NoError(t, err)
	require.NotNil(t, result.Asset)
	assert.Equal(t, int64(7), result.Asset.ID)
	assert.Nil(t, result.Upload, "a deduped presign must not hand out an upload URL")
	assert.Empty(t, h.store.signCalls, "dedupe must be decided before S3 is involved")
}

// TestPresignRejectsBeforeAnyS3Call covers every input the CHECK constraints
// would eventually reject. Reaching S3 first would mean signing a URL for an
// upload the database is guaranteed to refuse, and paying for the transfer.
func TestPresignRejectsBeforeAnyS3Call(t *testing.T) {
	valid := PresignRequest{Filename: "cart.png", ContentType: "image/png", Bytes: 4096, SHA256: testSHA}

	cases := []struct {
		name   string
		mutate func(*PresignRequest)
	}{
		{"oversized", func(r *PresignRequest) { r.Bytes = MaxBytes + 1 }},
		{"zero bytes", func(r *PresignRequest) { r.Bytes = 0 }},
		{"negative bytes", func(r *PresignRequest) { r.Bytes = -1 }},
		{"content type not an image", func(r *PresignRequest) { r.ContentType = "application/pdf" }},
		{"content type empty", func(r *PresignRequest) { r.ContentType = "" }},
		{"svg, which the schema also forbids", func(r *PresignRequest) { r.ContentType = "image/svg+xml" }},
		{"sha too short", func(r *PresignRequest) { r.SHA256 = "abc" }},
		{"sha not hex", func(r *PresignRequest) { r.SHA256 = "zz" + testSHA[2:] }},
		{"filename empty", func(r *PresignRequest) { r.Filename = "" }},
		{"filename only punctuation", func(r *PresignRequest) { r.Filename = "..." }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			req := valid
			tc.mutate(&req)

			_, err := h.svc.Presign(context.Background(), req, "token:ci")

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBadRequest, "caller errors must map to 400, not 500")
			assert.Empty(t, h.store.signCalls, "nothing may be signed for a rejected declaration")
		})
	}
}

// TestPresignRejectsWebpWithAnExplanation documents a real limitation rather
// than letting it surface as a 500. storage/v4's POST branch derives the
// policy's content type from a filename extension it does not recognise for
// webp, and the signing call fails outright.
func TestPresignRejectsWebp(t *testing.T) {
	h := newHarness()

	_, err := h.svc.Presign(context.Background(), PresignRequest{
		Filename: "cart.webp", ContentType: "image/webp", Bytes: 4096, SHA256: testSHA,
	}, "token:ci")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadRequest)
	assert.Contains(t, err.Error(), "image/webp")
	assert.Empty(t, h.store.signCalls)
}

// TestPresignUsesPostPolicy is the correctness detail this whole flow turns on.
//
// A presigned PUT ignores Options.ContentType and carries no conditions at all,
// so the browser could upload anything of any size to the key we signed and
// every validation in Presign would be decorative. Only the POST policy binds
// the declaration to what S3 will accept.
func TestPresignUsesPostPolicyAndBindsTheDeclaration(t *testing.T) {
	h := newHarness()
	h.store.signFields = map[string]string{"key": "screenshots/aa/bb/" + testSHA}

	result, err := h.svc.Presign(context.Background(), PresignRequest{
		Filename: "Ecran — cart.png", ContentType: "image/png", Bytes: 4096, SHA256: testSHA,
	}, "token:ci")

	require.NoError(t, err)
	require.NotNil(t, result.Upload)
	assert.Nil(t, result.Asset)

	require.Len(t, h.store.signCalls, 1)
	call := h.store.signCalls[0]

	assert.Equal(t, http.MethodPost, call.method, "presigned PUT cannot constrain the upload")
	assert.Equal(t, "screenshots/aa/bb/"+testSHA, call.key)

	// storage/v4 reads the policy's Content-Type from THIS argument's
	// extension, not from Options.ContentType, which it ignores.
	assert.Equal(t, testSHA+".png", call.filename)

	assert.Equal(t, "4096", call.opts.Metadata[metaDeclaredBytes])
	assert.Equal(t, "image/png", call.opts.Metadata[metaDeclaredContentType])
	// Non-ASCII is rewritten rather than refused; the filename is cosmetic.
	assert.Equal(t, "Ecran _ cart.png", call.opts.Metadata[metaDeclaredFilename])

	assert.Empty(t, h.assets.created, "presign must not create a row; only confirm may")
}

func TestPresignMapsJpegToJpgExtension(t *testing.T) {
	h := newHarness()

	_, err := h.svc.Presign(context.Background(), PresignRequest{
		Filename: "cart.jpeg", ContentType: "image/jpeg", Bytes: 10, SHA256: testSHA,
	}, "token:ci")

	require.NoError(t, err)
	require.Len(t, h.store.signCalls, 1)
	assert.Equal(t, testSHA+".jpg", h.store.signCalls[0].filename)
}

// --- confirm ---------------------------------------------------------------

// TestConfirmRefusesWhenTheObjectIsAbsent is the bug that matters. Without the
// HEAD, a client that skipped the upload entirely — or whose upload was
// rejected by the policy — still gets a row, and the row points at nothing.
func TestConfirmRefusesWhenTheObjectIsAbsent(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NoSuchBucket"} {
		t.Run(code, func(t *testing.T) {
			h := newHarness()
			h.store.attrsErr = minio.ErrorResponse{Code: code}

			_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUploadNotFound)
			assert.Empty(t, h.assets.created, "no row may be written for an object that is not there")
			assert.Empty(t, h.audit.events)
			assert.Equal(t, 0, h.tx.calls, "no transaction should even be opened")
		})
	}
}

// TestConfirmDoesNotMistakeAnOutageForAMissingObject keeps a genuine S3 fault
// out of the 4xx bucket, where it would be invisible to on-call.
func TestConfirmDoesNotMistakeAnOutageForAMissingObject(t *testing.T) {
	h := newHarness()
	h.store.attrsErr = errors.New("dial tcp: connection refused")

	_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUploadNotFound)
	assert.NotErrorIs(t, err, ErrBadRequest)
	assert.Empty(t, h.assets.created)
}

// TestConfirmRejectsSizeMismatch is the check the upload policy cannot make:
// storage/v4 sets no content-length-range condition, so a client may declare
// 4KB, take the signed form and push 50MB through it.
func TestConfirmRejectsSizeMismatch(t *testing.T) {
	h := newHarness()
	h.store.attrs = declaredAttrs(50<<20, "image/png", 4096, "image/png", "cart.png")

	_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUploadMismatch)
	assert.Contains(t, err.Error(), "4096")
	assert.Empty(t, h.assets.created)
	assert.Empty(t, h.audit.events)
}

func TestConfirmRejectsContentTypeMismatch(t *testing.T) {
	h := newHarness()
	h.store.attrs = declaredAttrs(4096, "application/zip", 4096, "image/png", "cart.png")

	_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUploadMismatch)
	assert.Empty(t, h.assets.created)
}

// TestConfirmToleratesADecoratedContentTypeHeader: a parameter or a different
// case is the same media type, and refusing a good upload over one would be a
// self-inflicted outage that only shows up against a particular proxy.
func TestConfirmToleratesADecoratedContentTypeHeader(t *testing.T) {
	for _, actual := range []string{"IMAGE/PNG", "image/png; charset=binary", "  image/png  "} {
		t.Run(actual, func(t *testing.T) {
			h := newHarness()
			h.store.attrs = declaredAttrs(4096, actual, 4096, "image/png", "cart.png")

			asset, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

			require.NoError(t, err)
			assert.Equal(t, "image/png", asset.ContentType)
		})
	}
}

// TestConfirmRejectsAnObjectOverTheLimitEvenWhenSelfConsistent guards against
// a declaration signed by an older or laxer build: the object agrees with what
// it claims, and both are still inadmissible.
func TestConfirmRejectsAnObjectOverTheLimitEvenWhenSelfConsistent(t *testing.T) {
	h := newHarness()
	oversized := MaxBytes + 1
	h.store.attrs = declaredAttrs(int64(oversized), "image/png", oversized, "image/png", "cart.png")

	_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUploadMismatch)
	assert.Empty(t, h.assets.created)
}

// TestConfirmRejectsAnObjectWithNoDeclaration refuses to admit an object that
// appeared in our key space without going through our presign. There is
// nothing to verify it against, and "no evidence" is not "verified".
func TestConfirmRejectsAnObjectWithNoDeclaration(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string][]string
	}{
		{"no metadata at all", nil},
		{"size missing", map[string][]string{
			"X-Amz-Meta-Declared-Content-Type": {"image/png"},
			"X-Amz-Meta-Declared-Filename":     {"cart.png"},
		}},
		{"size not a number", map[string][]string{
			"X-Amz-Meta-Declared-Bytes":        {"lots"},
			"X-Amz-Meta-Declared-Content-Type": {"image/png"},
			"X-Amz-Meta-Declared-Filename":     {"cart.png"},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.store.attrs = storage.ObjectAttributes{
				FileSize: 4096, ContentType: "image/png", Metadata: tc.metadata,
			}

			_, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUploadUnverifiable)
			assert.Empty(t, h.assets.created)
		})
	}
}

func TestConfirmRejectsAMalformedHash(t *testing.T) {
	h := newHarness()

	_, err := h.svc.Confirm(context.Background(), "not-a-hash", "token:ci", "req-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadRequest)
	assert.Empty(t, h.store.statCalls, "a malformed hash must not become an S3 key")
}

func TestConfirmCreatesTheRowAndAuditsIt(t *testing.T) {
	h := newHarness()
	h.store.attrs = declaredAttrs(4096, "image/png", 4096, "image/png", "cart.png")

	asset, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-9")

	require.NoError(t, err)
	assert.Equal(t, testSHA, asset.SHA256)
	assert.Equal(t, "cart.png", asset.Filename)
	assert.Equal(t, "image/png", asset.ContentType)
	assert.Equal(t, 4096, asset.Bytes)
	assert.Equal(t, "screenshots/aa/bb/"+testSHA, asset.S3Key)
	assert.Equal(t, "token:ci", asset.UploadedBy)

	// The row is inserted from the OBJECT's attributes, never from a second
	// client declaration.
	assert.Equal(t, []string{"screenshots/aa/bb/" + testSHA}, h.store.statCalls)

	require.Len(t, h.audit.events, 1)
	assert.Equal(t, repository.ActionAssetCreate, h.audit.events[0].Action)
	assert.Equal(t, "req-9", h.audit.events[0].RequestID)
	assert.Equal(t, 1, h.tx.calls, "the row and its audit row are one transaction")
}

// TestConfirmIsIdempotent: a retried confirm after a dropped response is a
// normal event, not an error, and must not re-stat the object.
func TestConfirmIsIdempotent(t *testing.T) {
	h := newHarness()
	h.assets.bySHA[testSHA] = repository.Asset{ID: 12, SHA256: testSHA}

	asset, err := h.svc.Confirm(context.Background(), testSHA, "token:ci", "req-1")

	require.NoError(t, err)
	assert.Equal(t, int64(12), asset.ID)
	assert.Empty(t, h.store.statCalls)
	assert.Empty(t, h.assets.created)
}

// --- presigned GET ---------------------------------------------------------

// TestSignedURLRefusesToIssueWhenTheAuditWriteFails is the reason the audit
// row is written first. These images carry customer PII; a URL handed out with
// no record of who received it defeats the only control that answers "who
// looked at this".
func TestSignedURLRefusesToIssueWhenTheAuditWriteFails(t *testing.T) {
	h := newHarness()
	h.assets.byID[5] = repository.Asset{ID: 5, SHA256: testSHA, S3Key: "screenshots/aa/bb/" + testSHA}
	h.audit.err = errors.New("audit table unavailable")

	url, err := h.svc.SignedURL(context.Background(), 5, "token:ci", "req-1")

	require.Error(t, err)
	assert.Empty(t, url)
	assert.Empty(t, h.store.signCalls, "no URL may be signed without a view record")
}

func TestSignedURLAuditsTheView(t *testing.T) {
	h := newHarness()
	h.assets.byID[5] = repository.Asset{ID: 5, SHA256: testSHA, S3Key: "screenshots/aa/bb/" + testSHA}
	h.store.signURL = "https://bucket.example/get?sig=secret"

	url, err := h.svc.SignedURL(context.Background(), 5, "token:ci", "req-1")

	require.NoError(t, err)
	assert.Equal(t, "https://bucket.example/get?sig=secret", url)

	require.Len(t, h.audit.events, 1)
	assert.Equal(t, repository.ActionAssetView, h.audit.events[0].Action)
	assert.Equal(t, "asset:5", h.audit.events[0].Target)
	assert.Equal(t, "token:ci", h.audit.events[0].Actor)

	require.Len(t, h.store.signCalls, 1)
	assert.Equal(t, http.MethodGet, h.store.signCalls[0].method)
}

func TestSignedURLUnknownAsset(t *testing.T) {
	h := newHarness()

	_, err := h.svc.SignedURL(context.Background(), 404, "token:ci", "req-1")

	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.Empty(t, h.audit.events)
	assert.Empty(t, h.store.signCalls)
}

// --- attach / detach -------------------------------------------------------

func TestAttachLinksAndAudits(t *testing.T) {
	h := newHarness()
	h.assets.keyIDs[100] = true
	h.assets.byID[5] = repository.Asset{ID: 5, SHA256: testSHA}

	err := h.svc.Attach(context.Background(), 100, 5, "truncates past 18 characters", "token:ci", "req-3")

	require.NoError(t, err)
	require.Len(t, h.assets.attached, 1)
	assert.Equal(t, int64(100), h.assets.attached[0].keyID)
	assert.Equal(t, int64(5), h.assets.attached[0].assetID)
	assert.Equal(t, "truncates past 18 characters", h.assets.attached[0].note)
	assert.Equal(t, "token:ci", h.assets.attached[0].by)

	require.Len(t, h.audit.events, 1)
	assert.Equal(t, repository.ActionAssetAttach, h.audit.events[0].Action)
	assert.Equal(t, int64(100), h.audit.events[0].Metadata["key_id"])
	assert.Equal(t, 1, h.tx.calls)
}

// TestAttachRejectsUnknownKeyBeforeTouchingTheLinkTable keeps a bad id a 404
// rather than a foreign key violation surfacing as a 500.
func TestAttachRejectsUnknownKey(t *testing.T) {
	h := newHarness()
	h.assets.byID[5] = repository.Asset{ID: 5}

	err := h.svc.Attach(context.Background(), 999, 5, "", "token:ci", "req-3")

	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.Empty(t, h.assets.attached)
	assert.Empty(t, h.audit.events)
}

func TestAttachRejectsUnknownAsset(t *testing.T) {
	h := newHarness()
	h.assets.keyIDs[100] = true

	err := h.svc.Attach(context.Background(), 100, 999, "", "token:ci", "req-3")

	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.Empty(t, h.assets.attached)
	assert.Empty(t, h.audit.events)
}

func TestAttachRejectsAnOversizedNote(t *testing.T) {
	h := newHarness()
	h.assets.keyIDs[100] = true
	h.assets.byID[5] = repository.Asset{ID: 5}

	err := h.svc.Attach(context.Background(), 100, 5, string(make([]byte, 1001)), "token:ci", "req-3")

	assert.ErrorIs(t, err, ErrBadRequest)
	assert.Equal(t, 0, h.tx.calls)
}

func TestDetachUnlinksAndAudits(t *testing.T) {
	h := newHarness()

	err := h.svc.Detach(context.Background(), 100, 5, "token:ci", "req-4")

	require.NoError(t, err)
	require.Len(t, h.assets.detached, 1)
	assert.Equal(t, int64(100), h.assets.detached[0].keyID)

	require.Len(t, h.audit.events, 1)
	assert.Equal(t, repository.ActionAssetDetach, h.audit.events[0].Action)
}

// TestDetachOfAnAbsentLinkIsNotFound: "there was nothing to unlink" and "I
// unlinked it" are different facts and a caller may need to tell them apart.
func TestDetachOfAnAbsentLink(t *testing.T) {
	h := newHarness()
	h.assets.detachErr = repository.ErrNotFound

	err := h.svc.Detach(context.Background(), 100, 5, "token:ci", "req-4")

	assert.ErrorIs(t, err, repository.ErrNotFound)
	assert.Empty(t, h.audit.events)
}

// --- pure helpers ----------------------------------------------------------

func TestNormaliseSHA256(t *testing.T) {
	upper := "AABBCCDDEEFF00112233445566778899AABBCCDDEEFF00112233445566778899"
	got, err := normaliseSHA256("  " + upper + "  ")
	require.NoError(t, err)
	// Uppercase must fold to the canonical form, or the same bytes produce two
	// keys, two rows, and dedupe stops working.
	assert.Equal(t, testSHA, got)

	for _, bad := range []string{"", "abc", testSHA + "0", "gg" + testSHA[2:]} {
		_, err := normaliseSHA256(bad)
		assert.ErrorIs(t, err, ErrBadRequest, "input %q", bad)
	}
}

func TestS3KeyFansOutOnPrefix(t *testing.T) {
	// S3 partitions on key prefix; a flat namespace concentrates every write
	// on one partition.
	assert.Equal(t, "screenshots/aa/bb/"+testSHA, s3Key(testSHA))
}

func TestSanitiseFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cart.png", "cart.png"},
		{"  cart.png  ", "cart.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\me\shot.png`, "shot.png"},
		{"Écran — 3.png", "cran _ 3.png"},
		{"a\nb.png", "a_b.png"},
	}
	for _, tc := range cases {
		got, err := sanitiseFilename(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got)
	}

	for _, bad := range []string{"", "   ", "///", "..."} {
		_, err := sanitiseFilename(bad)
		assert.ErrorIs(t, err, ErrBadRequest, "input %q", bad)
	}

	long, err := sanitiseFilename(repeat("a", 400) + ".png")
	require.NoError(t, err)
	assert.Len(t, long, maxFilenameLen)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
