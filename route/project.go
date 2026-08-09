package route

import (
	"net/http"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"

	"github.com/yougroupteam/u-l10n/pkg/model"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
)

// --- response shapes --------------------------------------------------------

type projectResponse struct {
	ID     int16  `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// LokaliseProjectID is omitted rather than sent as "": absent and blank
	// are different facts here too — a project with no Lokalise source at
	// all, versus one whose id happens to render as an empty string.
	LokaliseProjectID string `json:"lokalise_project_id,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func newProjectResponse(p model.Project) projectResponse {
	return projectResponse{
		ID:                p.ID,
		Code:              p.Code,
		Name:              p.Name,
		Status:            p.Status,
		LokaliseProjectID: p.LokaliseProjectID,
		CreatedAt:         formatTime(p.CreatedAt),
		UpdatedAt:         formatTime(p.UpdatedAt),
	}
}

type projectListResponse struct {
	Projects []projectResponse `json:"projects"`
}

// --- request shapes ---------------------------------------------------------

type createProjectRequest struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	LokaliseProjectID string `json:"lokalise_project_id"`
}

type patchProjectRequest struct {
	Name              string `json:"name"`
	Status            string `json:"status"`
	LokaliseProjectID string `json:"lokalise_project_id"`
}

// --- handlers ---------------------------------------------------------------

// ListProjects returns every live project.
//
//	GET /api/v1/projects
//
// Any provisioned operator, viewer and up: knowing which projects exist is
// not privileged — only creating or reconfiguring one is, which is why this
// route sits in the viewer group while CreateProject and PatchProject sit
// behind requirePlatformAdmin below.
func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	projects, err := h.projectSvc.List(r.Context(), false)
	if err != nil {
		h.projectError(w, r, "list projects", err)
		return
	}

	out := projectListResponse{Projects: make([]projectResponse, 0, len(projects))}
	for _, p := range projects {
		out.Projects = append(out.Projects, newProjectResponse(p))
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, out)
}

// CreateProject mints a new project.
//
//	POST /api/v1/projects
//	{"code":"youbiz","name":"YouBiz"}
//
// Platform admin only — the one privilege that is not scoped to a project,
// because no project exists yet for a per-project role to apply to. The
// creator receives the project's first grant, admin, in the same transaction
// that creates the row: see projectsvc.Create.
func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}

	var body createProjectRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	created, err := h.projectSvc.Create(r.Context(), identityActor(r.Context()), projectsvc.NewProject{
		Code:              body.Code,
		Name:              body.Name,
		LokaliseProjectID: body.LokaliseProjectID,
	})
	if err != nil {
		h.projectError(w, r, "create project", err)
		return
	}

	render.Status(r, http.StatusCreated)
	render.JSON(w, r, newProjectResponse(created))
}

// PatchProject changes a project's name, status or Lokalise linkage.
//
//	PATCH /api/v1/projects/{project}
//	{"name":"YouBiz","status":"active","lokalise_project_id":""}
//
// Platform admin only, like CreateProject. All three fields are required on
// every call — see projectsvc.ProjectPatch — because ProjectRepository.Update
// takes a full replacement, not a diff.
func (h *Handler) PatchProject(w http.ResponseWriter, r *http.Request) {
	if err := rejectUnknownParams(r); err != nil {
		h.badRequest(w, r, err)
		return
	}
	code := chi.URLParam(r, "project")

	var body patchProjectRequest
	if err := decodePortalJSON(w, r, &body); err != nil {
		h.badRequest(w, r, err)
		return
	}

	updated, err := h.projectSvc.Update(r.Context(), code, projectsvc.ProjectPatch{
		Name:              body.Name,
		Status:            body.Status,
		LokaliseProjectID: body.LokaliseProjectID,
	})
	if err != nil {
		h.projectError(w, r, "update project", err)
		return
	}

	render.Status(r, http.StatusOK)
	render.JSON(w, r, newProjectResponse(updated))
}

// projectError maps the project service's sentinels onto status codes.
//
// It delegates to the shared portalError mapper — see portal.go — rather
// than keeping its own switch, so the two cannot drift into disagreeing
// about what a caller mistake looks like. The name mirrors userError's,
// which exists for the same reason on the user-management routes: a
// resource-specific entry point that other files can call without knowing
// portalError handles it underneath.
func (h *Handler) projectError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.portalError(w, r, op, err)
}
