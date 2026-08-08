package route

import (
	"context"
	"net/http"

	"github.com/go-chi/render"
)

// Pinger is the narrowest view of a backing dependency this package needs:
// "are you reachable?". *sql.DB satisfies it, and so does a test double —
// which is why the handler depends on this rather than on SqlConnector.
type Pinger interface {
	PingContext(ctx context.Context) error
}

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

type readinessResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Healthz reports process liveness. It deliberately touches no dependency.
//
// Kubernetes restarts a pod whose liveness probe fails, so this must answer
// "is this process wedged?" and nothing else. Pinging the database here would
// mean a brief database blip restarts every pod in the fleet at once — turning
// a recoverable dependency outage into a self-inflicted one.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	render.Status(r, http.StatusOK)
	render.JSON(w, r, healthResponse{Status: "ok", Service: serviceName})
}

// Readyz reports whether this instance can serve traffic right now.
//
// Kubernetes removes a pod failing its readiness probe from the Service's
// endpoints but leaves it running. That is the correct response to an
// unreachable database: stop sending it work, let it recover, restart nothing.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	if err := h.db.PingContext(r.Context()); err != nil {
		log.Errore(r.Context(), "readiness check failed: database unreachable", err)
		render.Status(r, http.StatusServiceUnavailable)
		render.JSON(w, r, readinessResponse{
			Status: "unavailable",
			Checks: map[string]string{"database": "unreachable"},
		})
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, readinessResponse{
		Status: "ready",
		Checks: map[string]string{"database": "ok"},
	})
}
