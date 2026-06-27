package dashboard

import (
	"errors"
	"fmt"
	"html/template"

	"github.com/labstack/echo/v4"
)

// indexReq is the body for POST /api/v1/index.
type indexReq struct {
	Project  string `json:"project"`   // project name (must exist in store)
	RootPath string `json:"root_path"` // optional override; defaults to stored root
}

// indexAPI triggers an indexing run for a project and returns stats as JSON.
func (s *Server) indexAPI(c echo.Context) error {
	var req indexReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Project == "" {
		return c.JSON(400, map[string]string{"error": "project is required"})
	}
	rootPath, err := s.resolveRootPath(c, req.Project, req.RootPath)
	if err != nil {
		return c.JSON(404, map[string]string{"error": err.Error()})
	}
	if s.indexer == nil {
		return c.JSON(503, map[string]string{"error": "indexer unavailable (embedder/qdrant offline)"})
	}
	stats, err := s.indexer.IndexProject(c.Request().Context(), req.Project, rootPath)
	if err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, stats)
}

// uiRunIndex triggers indexing via the HTMX UI and returns an HTML stats partial.
func (s *Server) uiRunIndex(c echo.Context) error {
	project := c.FormValue("project")
	if project == "" {
		return c.HTML(400, "<p class='error'>Project is required</p>")
	}
	rootPath, err := s.resolveRootPath(c, project, "")
	if err != nil {
		return c.HTML(404, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	if s.indexer == nil {
		return c.HTML(503, "<p class='error'>Indexer unavailable (Qdrant/embedder offline).</p>")
	}
	stats, err := s.indexer.IndexProject(c.Request().Context(), project, rootPath)
	if err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	return c.HTML(200, s.renderIndexStats(stats))
}

// resolveRootPath looks up the project's root path from the store, or uses
// the override if provided.
func (s *Server) resolveRootPath(c echo.Context, project, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	// Find project by name in the store.
	projects, err := s.store.ListProjects(c.Request().Context())
	if err != nil {
		return "", fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if p.Name == project {
			return p.RootPath, nil
		}
	}
	return "", fmt.Errorf("project %q not found", project)
}

// renderIndexStats renders the indexing result as an HTML partial.
func (s *Server) renderIndexStats(stats interface{}) string {
	// Use type switch via fmt to avoid importing indexer types in the render layer.
	st := fmt.Sprintf("%+v", stats)
	return fmt.Sprintf("<div class='index-stats'><h3>Indexing Complete</h3><pre>%s</pre></div>",
		template.HTMLEscapeString(st))
}

// guard to keep errors import if used only in type assertions later.
var _ = errors.Is
