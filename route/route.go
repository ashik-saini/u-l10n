// Package route wires the HTTP surface of u-l10n.
//
// Handlers in this package decode and validate requests, delegate to
// pkg/service, and map errors onto HTTP status codes. They contain no business
// rules and issue no SQL — see pkg/service and pkg/repository respectively.
package route

import (
	"net/http"

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
	"github.com/yougroupteam/u-l10n/pkg/service/exportsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/keysvc"
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
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(render.SetContentType(render.ContentTypeJSON))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(cnf.RequestTimeout))

	// Probe endpoints sit outside /api and outside authentication: kubelet
	// presents no credentials, and a probe that can fail for auth reasons is
	// a probe that reports the wrong thing.
	r.Get("/healthz", handler.Healthz)
	r.Get("/readyz", handler.Readyz)

	// Mounted at /api/v1; the gateway exposes it to the portal and to scripts
	// as /api/l10n/*.
	r.Route("/api/v1", func(r chi.Router) {
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
		})

		// Granting privileges requires holding them.
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireIdentity(repository.RoleAdmin))
			r.Patch("/admin/users/{email}/role", handler.SetUserRole)
		})
	})

	// OTA sits on its own prefix, OUTSIDE /api/v1 and outside authentication.
	// The app calls it at launch before login, and the payload already ships
	// inside the binary — see the handler for the full reasoning.
	r.Route("/ota/v1", func(r chi.Router) {
		r.Get("/bundles/{locale}", handler.OTABundle)
	})

	return r
}
