package dashboard

import (
	"errors"
	"fmt"
	"html/template"
	"strconv"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
	"github.com/labstack/echo/v4"
)

// --- HTMX UI handlers ---
//
// These render server-side HTML fragments. The main page (/) serves the full
// shell; HTMX partials (under /ui/*) swap content regions (sidebar list,
// search results). No client-side JS framework — just HTMX attributes.

// uiIndex renders the full dashboard shell.
func (s *Server) uiIndex(c echo.Context) error {
	projects, _ := s.store.ListProjects(c.Request().Context())
	return c.HTML(200, s.renderShell(projects))
}

// uiProjectList renders only the sidebar project list (HTMX swap target).
func (s *Server) uiProjectList(c echo.Context) error {
	projects, err := s.store.ListProjects(c.Request().Context())
	if err != nil {
		return c.HTML(500, "<p class='error'>Failed to load projects</p>")
	}
	return c.HTML(200, s.renderProjectList(projects))
}

// uiProjectDetail renders the detail panel for a project.
func (s *Server) uiProjectDetail(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.HTML(400, "<p class='error'>Invalid project ID</p>")
	}
	p, err := s.store.GetProject(c.Request().Context(), id)
	if err != nil {
		return c.HTML(404, "<p class='error'>Project not found</p>")
	}
	return c.HTML(200, s.renderProjectDetail(p))
}

// uiCreateProject handles the create form submit, then returns the refreshed list.
func (s *Server) uiCreateProject(c echo.Context) error {
	name := c.FormValue("name")
	rootPath := c.FormValue("root_path")
	desc := c.FormValue("description")
	domains := c.FormValue("domains")
	if name == "" || rootPath == "" {
		return c.HTML(400, "<p class='error'>Name and root path are required</p>")
	}
	if domains == "" {
		domains = "code"
	}
	_, err := s.store.CreateProject(c.Request().Context(), store.Project{
		Name: name, RootPath: rootPath, Description: desc, Domains: domains,
	})
	if err != nil {
		return c.HTML(400, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	// Return refreshed list.
	projects, _ := s.store.ListProjects(c.Request().Context())
	return c.HTML(200, s.renderProjectList(projects))
}

// uiSearch renders search results for the HTMX search panel.
// Reads query + project from query params (GET, HTMX hx-get).
func (s *Server) uiSearch(c echo.Context) error {
	query := c.QueryParam("q")
	project := c.QueryParam("project")
	if query == "" || project == "" {
		return c.HTML(200, "<p class='muted'>Enter a query and select a project.</p>")
	}
	results, err := s.doSearch(c.Request().Context(), project, query, 5)
	if err != nil {
		if errors.Is(err, errBackendUnavailable) {
			return c.HTML(503, "<p class='error'>Search backend (Qdrant/embedder) offline. Start Qdrant to enable search.</p>")
		}
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	return c.HTML(200, s.renderResults(query, results))
}

// --- Template rendering (string-based, no external template files needed) ---

func (s *Server) renderShell(projects []store.Project) string {
	var sb string
	sb = "<!DOCTYPE html><html><head><meta charset='utf-8'>"
	sb += "<title>bit-multi-brain-rag</title>"
	sb += "<meta name='viewport' content='width=device-width,initial-scale=1'>"
	sb += "<script src='https://unpkg.com/htmx.org@1.9.12'></script>"
	sb += "<style>" + cssShell + "</style>"
	sb += "</head><body>"
	sb += "<div class='layout'>"
	// Sidebar
	sb += "<aside class='sidebar'>"
	sb += "<h1>bit-multi-brain-rag</h1>"
	sb += s.renderProjectList(projects)
	sb += "<details class='add-form'><summary>+ Add Project</summary>"
	sb += "<form hx-post='/ui/projects' hx-target='#project-list' class='form'>"
	sb += "<input name='name' placeholder='Project name' required>"
	sb += "<input name='root_path' placeholder='D:\\path\\to\\repo' required>"
	sb += "<input name='description' placeholder='Description (optional)'>"
	sb += "<input name='domains' value='code' placeholder='code,doc,task'>"
	sb += "<button type='submit'>Create</button>"
	sb += "</form></details>"
	sb += "</aside>"
	// Main
	sb += "<main class='main' id='main'>"
	sb += "<div class='search-bar'>"
	sb += "<form hx-get='/ui/search' hx-target='#results' class='search-form'>"
	sb += "<input name='q' id='search-input' placeholder='Search code...'>"
	sb += "<select name='project' id='project-select'>"
	for _, p := range projects {
		sb += fmt.Sprintf("<option value='%s'>%s</option>", template.HTMLEscapeString(p.Name), template.HTMLEscapeString(p.Name))
	}
	sb += "</select>"
	sb += "<button type='submit'>Search</button>"
	sb += "</form>"
	sb += "</div>"
	sb += "<div id='results'><p class='muted'>Select a project and type a query.</p></div>"
	sb += "</main>"
	sb += "</div></body></html>"
	return sb
}

func (s *Server) renderProjectList(projects []store.Project) string {
	sb := "<div id='project-list' class='project-list'>"
	if len(projects) == 0 {
		sb += "<p class='muted small'>No projects yet.</p>"
	} else {
		for _, p := range projects {
			sb += fmt.Sprintf(
				"<div class='project-item' hx-get='/ui/projects/%d' hx-target='#main'><span class='proj-name'>%s</span><span class='proj-domains'>%s</span></div>",
				p.ID, template.HTMLEscapeString(p.Name), template.HTMLEscapeString(p.Domains),
			)
		}
	}
	sb += "</div>"
	return sb
}

func (s *Server) renderProjectDetail(p store.Project) string {
	sb := "<div id='main' class='main'>"
	sb += "<div class='search-bar'>"
	sb += fmt.Sprintf("<form hx-get='/ui/search' hx-target='#results' class='search-form'>")
	sb += fmt.Sprintf("<input name='q' placeholder='Search in %s...' autofocus>", template.HTMLEscapeString(p.Name))
	sb += fmt.Sprintf("<input type='hidden' name='project' value='%s'>", template.HTMLEscapeString(p.Name))
	sb += "<button type='submit'>Search</button></form></div>"
	sb += "<div class='project-info'>"
	sb += fmt.Sprintf("<h2>%s</h2>", template.HTMLEscapeString(p.Name))
	sb += fmt.Sprintf("<p class='muted'>%s</p>", template.HTMLEscapeString(p.RootPath))
	if p.Description != "" {
		sb += fmt.Sprintf("<p>%s</p>", template.HTMLEscapeString(p.Description))
	}
	sb += fmt.Sprintf("<span class='badge'>%s</span>", template.HTMLEscapeString(p.Domains))
	sb += "</div><div id='results'></div></div>"
	return sb
}

func (s *Server) renderResults(query string, results []rag.Result) string {
	sb := fmt.Sprintf("<h3>Results for &ldquo;%s&rdquo;</h3>", template.HTMLEscapeString(query))
	if len(results) == 0 {
		sb += "<p class='muted'>No results found.</p>"
		return sb
	}
	for _, r := range results {
		sb += "<div class='result-item'>"
		sb += fmt.Sprintf("<div class='result-meta'><span class='result-file'>%s</span><span class='result-score'>%.3f</span></div>",
			template.HTMLEscapeString(orFallback(r.Meta["source_file"], "(unknown)")), r.Score)
		sb += fmt.Sprintf("<pre class='result-content'>%s</pre>", template.HTMLEscapeString(truncateContent(r.Content, 500)))
		sb += "</div>"
	}
	return sb
}

// helper: first non-empty string.
func orFallback(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncateContent(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

// cssShell holds the dashboard stylesheet (inline to avoid external file deps).
const cssShell = `
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:#0d1117;color:#c9d1d9}
.layout{display:flex;height:100vh}
.sidebar{width:280px;background:#161b22;border-right:1px solid #30363d;padding:16px;overflow-y:auto}
.sidebar h1{font-size:18px;margin-bottom:16px;color:#58a6ff}
.project-list{margin-bottom:16px}
.project-item{padding:8px 12px;border-radius:6px;cursor:pointer;display:flex;justify-content:space-between;align-items:center}
.project-item:hover{background:#21262d}
.proj-name{font-weight:500}
.proj-domains{font-size:11px;color:#8b949e;background:#21262d;padding:2px 6px;border-radius:10px}
.main{flex:1;padding:24px;overflow-y:auto}
.search-bar{margin-bottom:24px}
.search-form{display:flex;gap:8px}
.search-form input[type=text]{flex:1;padding:10px 14px;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#c9d1d9;font-size:14px}
.search-form input:not([type=text]),.search-form button{padding:10px 16px;background:#238636;border:none;border-radius:6px;color:#fff;cursor:pointer;font-size:14px}
.search-form select{padding:10px;background:#161b22;border:1px solid #30363d;border-radius:6px;color:#c9d1d9}
.add-form{margin-top:16px;padding-top:16px;border-top:1px solid #30363d}
.add-form summary{cursor:pointer;color:#58a6ff;font-size:13px;margin-bottom:8px}
.form{display:flex;flex-direction:column;gap:8px}
.form input{padding:8px;background:#0d1117;border:1px solid #30363d;border-radius:4px;color:#c9d1d9;font-size:13px}
.form button{padding:8px;background:#238636;border:none;border-radius:4px;color:#fff;cursor:pointer}
.result-item{background:#161b22;border:1px solid #30363d;border-radius:6px;padding:16px;margin-bottom:12px}
.result-meta{display:flex;justify-content:space-between;margin-bottom:8px}
.result-file{font-family:monospace;font-size:12px;color:#58a6ff}
.result-score{font-size:12px;color:#8b949e}
.result-content{font-family:'Cascadia Code',Consolas,monospace;font-size:12px;white-space:pre-wrap;background:#0d1117;padding:12px;border-radius:4px;overflow-x:auto;color:#adbac7}
.muted{color:#8b949e}
.small{font-size:12px}
.error{color:#f85149}
.badge{display:inline-block;background:#21262d;color:#8b949e;padding:4px 10px;border-radius:10px;font-size:11px}
.project-info{margin-bottom:24px;padding-bottom:16px;border-bottom:1px solid #30363d}
`
