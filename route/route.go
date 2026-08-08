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
	cnf *config.Config
	db  Pinger
}

func ProvideHandler(cnf *config.Config, sqlConnector database.SqlConnector) *Handler {
	return &Handler{
		cnf: cnf,
		db:  sqlConnector.GetDB(),
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

	return r
}
