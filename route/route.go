// Package route wires the HTTP surface of u-l10n.
//
// Handlers in this package decode and validate requests, delegate to
// pkg/service, and map errors onto HTTP status codes. They contain no business
// rules and issue no SQL — see pkg/service and pkg/repository respectively.
package route

import (
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
	"github.com/google/wire"
	"go.elastic.co/apm/module/apmchi"

	"github.com/yougroupteam/u-common-components/apm"
	"github.com/yougroupteam/u-common-components/database"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/googleauth"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/assetsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/branchsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/exportsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mrsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/releasesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/tagsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
)

const serviceName = "u-l10n"

var (
	log = ulog.GetLogger(serviceName)

	WireSet = wire.NewSet(
		ProvideHandler,
		ProvideRoutes,
	)
)

// Handler carries the dependencies shared by every HTTP handler in this
// package. One struct per service, not per endpoint — endpoints live in their
// own files and hang methods off this type.
type Handler struct {
	cnf      *config.Config
	db       Pinger
	exports  *exportsvc.Service
	tokens   repository.APITokenRepository
	locales  repository.LocaleRepository
	releases repository.ReleaseRepository
	assets   *assetsvc.Service
	// identities and users are the two halves of a human principal: Google
	// answers who, the users table answers what they may do. See identity.go.
	identities TokenVerifier
	users      repository.UserRepository
	userSvc    *usersvc.Service

	// keySvc backs the portal's key browser and inline editor.
	keySvc *keysvc.Service

	// branchSvc backs the branch list and the branch diff.
	branchSvc *branchsvc.Service

	// mrSvc backs the review workflow; the merge itself belongs to mergesvc.
	mrSvc *mrsvc.Service

	// tagSvc backs the tag manager, and audits every tag mutation.
	tagSvc *tagsvc.Service

	// releaseSvc backs the release history, the manual publish and the OTA
	// kill switch.
	releaseSvc *releasesvc.Service

	// projectSvc backs the project list every operator can read, and the
	// create/patch pair gated by requirePlatformAdmin below — minting a
	// project and granting its first, admin role is the one privilege that
	// is not scoped to a project.
	projectSvc *projectsvc.Service
}

func ProvideHandler(
	cnf *config.Config,
	sqlConnector database.SqlConnector,
	exports *exportsvc.Service,
	tokens repository.APITokenRepository,
	locales repository.LocaleRepository,
	releases repository.ReleaseRepository,
	assets *assetsvc.Service,
	identities *googleauth.Verifier,
	users repository.UserRepository,
	userSvc *usersvc.Service,
	keySvc *keysvc.Service,
	branchSvc *branchsvc.Service,
	mrSvc *mrsvc.Service,
	tagSvc *tagsvc.Service,
	releaseSvc *releasesvc.Service,
	projectSvc *projectsvc.Service,
) *Handler {
	return &Handler{
		cnf:        cnf,
		db:         sqlConnector.GetDB(),
		exports:    exports,
		tokens:     tokens,
		locales:    locales,
		releases:   releases,
		assets:     assets,
		identities: identities,
		users:      users,
		userSvc:    userSvc,
		keySvc:     keySvc,
		branchSvc:  branchSvc,
		mrSvc:      mrSvc,
		tagSvc:     tagSvc,
		releaseSvc: releaseSvc,
		projectSvc: projectSvc,
	}
}

// ProvideRoutes builds the chi router and its middleware chain.
//
// Middleware order is load-bearing and matches the house chain in u-reward:
// tracing and request identity first so that everything downstream — including
// panics and timeouts — is attributable to a request.
func ProvideRoutes(apmConfig *apm.ApmConfig, cnf *config.Config, handler *Handler) http.Handler {
	r := chi.NewRouter()

	if apmConfig.Enable {
		r.Use(apmchi.Middleware())
	}
	// The socket address must be captured BEFORE RealIP overwrites RemoteAddr
	// with client-chosen headers: the rate limiter keys on it (see clientIP),
	// and after RealIP runs it is unrecoverable. RealIP stays for logging.
	r.Use(captureSocketAddr)
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(render.SetContentType(render.ContentTypeJSON))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(cnf.RequestTimeout))

	// One limiter shared by every governed group, so a client cannot multiply
	// its budget by spreading requests across prefixes. It runs BEFORE any
	// authentication because the expensive paths it guards are pre-auth: an
	// unseen bearer token costs an outbound Google call with a 10-second
	// timeout, and an X-Api-Token costs a database lookup. The health probes
	// are exempt by MOUNTING, not configuration — a kubelet that gets 429 from
	// a liveness check restarts a healthy pod, turning overload into outage.
	limiter := newIPRateLimiter(cnf, time.Now)

	// Probe endpoints sit outside /api and outside authentication: kubelet
	// presents no credentials, and a probe that can fail for auth reasons is
	// a probe that reports the wrong thing.
	r.Get("/healthz", handler.Healthz)
	r.Get("/readyz", handler.Readyz)

	// Mounted at /api/v1; the gateway exposes it to the portal and to scripts
	// as /api/l10n/*.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(limiter.middleware)

		// Scripts and CI authenticate with X-Api-Token. read_export is the
		// minimum, so a token issued for pulling translations cannot be used to
		// write them.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireAPIToken(repository.ScopeReadExport))
			r.Get("/export", handler.Export)
		})

		// Context screenshots. read_write for every one of them, including the
		// presigned GET: these images carry customer PII, and a token issued
		// only to pull translations has no business reading them.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireAPIToken(repository.ScopeReadWrite))
			r.Post("/assets/presign", handler.AssetPresign)
			r.Post("/assets/confirm", handler.AssetConfirm)
			r.Get("/assets/{id}/url", handler.AssetURL)
			r.Put("/keys/{id}/assets", handler.AssetAttach)
			r.Delete("/keys/{id}/assets/{assetId}", handler.AssetDetach)
		})

		// Humans authenticate with the portal's Google access token. Viewer is
		// the floor: being provisioned at all is what /me reports, and the
		// portal gates its UI on that answer rather than on the x-yp-role
		// header it also sends — which is ignored here.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleViewer))
			r.Get("/me", handler.Me)
		})

		// Reading the corpus. Viewer, because seeing the copy that ships in the
		// app is the least a provisioned operator can do, and a translator who
		// cannot read the existing strings cannot write consistent ones.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleViewer))
			r.Get("/keys", handler.ListKeys)
			r.Get("/keys/{id}", handler.GetKey)
			r.Get("/keys/{id}/history", handler.KeyHistory)

			// Branches are read at viewer too: seeing what work is in flight,
			// and what a proposed change would do, is reading.
			r.Get("/branches", handler.ListBranches)
			r.Get("/branches/{name}", handler.GetBranch)
			r.Get("/branches/{name}/changes", handler.BranchChanges)

			r.Get("/merge-requests", handler.ListMergeRequests)
			r.Get("/merge-requests/{id}", handler.GetMergeRequest)
			r.Get("/merge-requests/{id}/conflicts", handler.MergeRequestConflicts)

			r.Get("/tags", handler.ListTags)

			// Knowing what shipped is reading. Changing what ships is not — see
			// the approver group.
			r.Get("/releases", handler.ListReleases)
			r.Get("/releases/{version}", handler.GetRelease)
			r.Get("/releases/{version}/bundles/{locale}", handler.ReleaseBundle)

			// Knowing which projects exist is reading, not administering
			// one — creating or reconfiguring a project sits behind the
			// platform-admin group below.
			r.Get("/projects", handler.ListProjects)
		})

		// Writing the corpus. Editor, and no higher: writing to a BRANCH is the
		// safe act by construction — nothing reaches an app until a merge, and a
		// merge needs an approver.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleEditor))
			r.Post("/keys", handler.CreateKey)
			r.Patch("/keys/{id}", handler.PatchKey)
			r.Delete("/keys/{id}", handler.DeleteKey)
			r.Put("/keys/{id}/translations/{locale}", handler.PutTranslation)
			r.Delete("/keys/{id}/translations/{locale}", handler.DeleteTranslation)

			r.Post("/branches", handler.CreateBranch)
			r.Post("/branches/{name}/close", handler.CloseBranch)
			r.Post("/branches/{name}/reopen", handler.ReopenBranch)

			// Asking for review, withdrawing the request, and recording which
			// side of a conflict wins are all authoring acts. Deciding whether
			// the result ships is not — see the approver group below.
			r.Post("/merge-requests", handler.CreateMergeRequest)
			r.Post("/merge-requests/{id}/reopen", handler.ReopenMergeRequest)
			r.Post("/merge-requests/{id}/close", handler.CloseMergeRequest)
			r.Put("/merge-requests/{id}/resolutions", handler.PutMergeRequestResolutions)

			// Tags are workflow metadata, not translatable content: they are
			// global per key rather than branch-scoped, so these routes take no
			// ?branch= and never enter a merge.
			r.Post("/tags", handler.CreateTag)
			r.Put("/tags/{id}", handler.UpdateTag)
			r.Delete("/tags/{id}", handler.DeleteTag)
			r.Post("/tags/{id}/keys", handler.AssignTag)
			r.Delete("/tags/{id}/keys", handler.UnassignTag)
			r.Put("/keys/{id}/tags", handler.PutKeyTags)
		})

		// Approving and merging. This is the boundary where copy stops being a
		// proposal and starts being what customers read, so it needs the role
		// that exists for exactly that judgement.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleApprover))
			r.Post("/merge-requests/{id}/approve", handler.ApproveMergeRequest)
			r.Post("/merge-requests/{id}/request-changes", handler.RequestMergeRequestChanges)
			r.Post("/merge-requests/{id}/reject", handler.RejectMergeRequest)
			r.Post("/merge-requests/{id}/merge", handler.MergeMergeRequest)

			// A manual publish ships master with no diff reviewed, and a
			// rollback withdraws shipped copy from every client. Both belong to
			// the role that exists for exactly that judgement.
			r.Post("/releases", handler.PublishRelease)
			r.Post("/releases/{version}/rollback", handler.RollbackRelease)
		})

		// Granting privileges requires holding them.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleAdmin))
			r.Patch("/admin/users/{email}/role", handler.SetUserRole)
		})

		// Minting a project, and granting its first role, is the one
		// privilege that is not scoped to a project — no per-project role
		// can apply to a project that does not exist yet. requirePlatformAdmin
		// runs after RequireIdentity and reads the flag it already loaded.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleViewer))
			r.Use(handler.requirePlatformAdmin)
			r.Post("/projects", handler.CreateProject)
			r.Patch("/projects/{project}", handler.PatchProject)
		})
	})

	// OTA sits on its own prefix, OUTSIDE /api/v1 and outside authentication.
	// The app calls it at launch before login, and the payload already ships
	// inside the binary — see the handler for the full reasoning.
	r.Route("/ota/v1", func(r chi.Router) {
		// The OTA design names rate limiting as one of its stated controls,
		// alongside the CDN. The CDN is the first line; this is the one that
		// holds when a caller reaches the service directly.
		r.Use(limiter.middleware)
		r.Get("/bundles/{locale}", handler.OTABundle)
	})

	return r
}
