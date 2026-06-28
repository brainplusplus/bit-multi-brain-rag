package dashboard

import (
	"errors"
	"fmt"
	"html/template"
	"strconv"
	"strings"

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

// uiProjectDetailPage renders the full shell with a project detail as active content.
func (s *Server) uiProjectDetailPage(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.Redirect(302, "/projects")
	}
	p, err := s.store.GetProject(c.Request().Context(), id)
	if err != nil {
		return c.Redirect(302, "/projects")
	}
	projects, _ := s.store.ListProjects(c.Request().Context())
	shell := s.renderShell(projects)

	// Build the detail content (same as HTMX partial)
	var jobPartial string
	if s.jobs != nil {
		if job, jerr := s.jobs.GetLatest(c.Request().Context(), p.Name); jerr == nil && job != nil {
			jobPartial = s.renderJobStatus(job)
		}
	}
	if jobPartial == "" {
		jobPartial = emptyIndexStats()
	}
	detailHTML := s.renderProjectDetail(p, jobPartial)

	// Inject into main content area, replacing default search bar
	shell = strings.Replace(shell, "<main class='main' id='main'>", "<main class='main' id='main'>"+detailHTML, 1)
	return c.HTML(200, shell)
}

// uiSettingsPage renders the full shell with settings as active content.
func (s *Server) uiSettingsPage(c echo.Context) error {
	projects, _ := s.store.ListProjects(c.Request().Context())
	shell := s.renderShell(projects)
	// The settings handler returns HTML partial — render it inline.
	// We trick it by calling the same handler and using its response.
	settingsHTML := s.renderSettingsHTML(c)
	shell = strings.Replace(shell, "<main class='main' id='main'>", "<main class='main' id='main'>"+settingsHTML, 1)
	return c.HTML(200, shell)
}

// renderSettingsHTML calls the settings rendering logic and returns HTML string.
func (s *Server) renderSettingsHTML(c echo.Context) string {
	// Delegate to a shared renderer that returns string (not echo response)
	return s.buildSettingsHTML(c)
}

// uiModelsPage renders the full shell with models as active content.
func (s *Server) uiModelsPage(c echo.Context) error {
	projects, _ := s.store.ListProjects(c.Request().Context())
	shell := s.renderShell(projects)
	// Trigger models load via HTMX after page render
	shell = strings.Replace(shell, "<main class='main' id='main'>",
		"<main class='main' id='main' hx-get='/ui/models' hx-trigger='load' hx-target='this' hx-swap='innerHTML'>", 1)
	return c.HTML(200, shell)
}

// uiProjectList renders only the sidebar project list (HTMX swap target).
func (s *Server) uiProjectList(c echo.Context) error {
	projects, err := s.store.ListProjects(c.Request().Context())
	if err != nil {
		return c.HTML(500, "<p class='error'>Failed to load projects</p>")
	}
	return c.HTML(200, s.renderProjectList(projects))
}

// uiProjectDetail renders the detail panel for a project. The panel embeds
// the latest job status (active or most-recent terminal) so a mid-flight
// page refresh resumes live polling without user action (ADR-0005).
func (s *Server) uiProjectDetail(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.HTML(400, "<p class='error'>Invalid project ID</p>")
	}
	p, err := s.store.GetProject(c.Request().Context(), id)
	if err != nil {
		return c.HTML(404, "<p class='error'>Project not found</p>")
	}
	// Best-effort fetch of latest job. Failure is non-fatal — we just render
	// the empty placeholder.
	var jobPartial string
	if s.jobs != nil {
		if job, jerr := s.jobs.GetLatest(c.Request().Context(), p.Name); jerr == nil && job != nil {
			jobPartial = s.renderJobStatus(job)
		}
	}
	if jobPartial == "" {
		jobPartial = emptyIndexStats()
	}
	return c.HTML(200, s.renderProjectDetail(p, jobPartial))
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
		MachineID:   c.Request().Header.Get("X-Machine-ID"),
		MachineName: c.Request().Header.Get("X-Machine-Name"),
		MachineOS:   c.Request().Header.Get("X-Machine-OS"),
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
	sb = "<!DOCTYPE html><html lang='en'><head><meta charset='utf-8'>"
	sb += "<title>BitBrain RAG</title>"
	sb += "<meta name='viewport' content='width=device-width,initial-scale=1'>"
	sb += "<link rel='preconnect' href='https://fonts.googleapis.com'>"
	sb += "<link rel='preconnect' href='https://fonts.gstatic.com' crossorigin>"
	sb += "<link href='https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&family=JetBrains+Mono:wght@400;500;600&display=swap' rel='stylesheet'>"
	sb += "<script src='https://unpkg.com/htmx.org@1.9.12'></script>"
	sb += "<style>" + cssShell + "</style>"
	sb += "</head><body>"
	sb += "<div class='layout'>"

	// Sidebar
	sb += "<aside class='sidebar'>"
	sb += "<div class='sidebar-head'>"
	sb += "<div class='sidebar-brand'>"
	// Inline SVG: neural network node icon
	sb += "<svg viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2'><circle cx='12' cy='5' r='2.5'/><circle cx='5' cy='19' r='2.5'/><circle cx='19' cy='19' r='2.5'/><line x1='12' y1='7.5' x2='5' y2='16.5'/><line x1='12' y1='7.5' x2='19' y2='16.5'/><line x1='5' y1='19' x2='19' y2='19'/></svg>"
	sb += "<h1>Bit<span>Brain</span> RAG</h1>"
	sb += "</div></div>"

	sb += "<div class='sidebar-scroll'>"
	// Health widget
	sb += "<div id='health-widget' class='health-widget' hx-get='/ui/health' hx-trigger='load, every 30s' hx-swap='outerHTML'><p class='muted small'>Connecting...</p></div>"

	// Navigation
	sb += "<nav class='sidebar-nav'>"
	sb += "<a href='/models' hx-get='/ui/models' hx-target='#main' hx-swap='innerHTML' hx-push-url='true' class='nav-link'><span class='nav-icon'>◈</span> Models</a>"
	sb += "<a href='/settings' hx-get='/ui/settings' hx-target='#main' hx-swap='innerHTML' hx-push-url='true' class='nav-link'><span class='nav-icon'>⚙</span> Settings</a>"
	sb += "</nav>"

	// Projects
	sb += "<div class='sidebar-section'>"
	sb += "<div class='sidebar-section-label'>Projects</div>"
	sb += s.renderProjectList(projects)
	sb += "</div>"
	sb += "</div>" // /sidebar-scroll

	// Add project form at bottom
	sb += "<details class='add-form'><summary>+ New project</summary>"
	sb += "<form hx-post='/ui/projects' hx-target='#project-list' class='form'>"
	sb += "<input name='name' placeholder='Project name' required>"
	sb += "<input name='root_path' placeholder='Path to repository' required>"
	sb += "<input name='description' placeholder='Description'>"
	sb += "<input name='domains' value='code' placeholder='Domains (code,doc,task)'>"
	sb += "<button type='submit'>Create</button>"
	sb += "</form></details>"
	sb += "</aside>"

	// Main content
	sb += "<main class='main' id='main'>"
	sb += "<div class='search-bar'>"
	sb += "<form hx-get='/ui/search' hx-target='#results' class='search-form'>"
	sb += "<input name='q' id='search-input' placeholder='Search across your codebase...' type='text'>"
	sb += "<select name='project' id='project-select'>"
	for _, p := range projects {
		sb += fmt.Sprintf("<option value='%s'>%s</option>", template.HTMLEscapeString(p.Name), template.HTMLEscapeString(p.Name))
	}
	sb += "</select>"
	sb += "<button type='submit'>Search</button>"
	sb += "</form></div>"
	sb += "<div id='results'><p class='muted' style='margin-top:40px;text-align:center'>Select a project from the sidebar, then search with natural language.</p></div>"
	sb += "</main>"
	// Drawer backdrop (sibling of drawer; toggled together)
	sb += "<div id='chunk-drawer-backdrop' class='chunk-drawer-backdrop' onclick='closeDrawer()'></div>"
	sb += "</div>"
	// Drawer open/close helpers + ESC handler
	sb += "<script>" + drawerJS + "</script>"
	sb += "</body></html>"
	return sb
}

const drawerJS = `
function openDrawer(triggerEl){
  var d=document.getElementById('chunk-drawer');
  var b=document.getElementById('chunk-drawer-backdrop');
  if(d){d.classList.add('open');d.setAttribute('aria-hidden','false')}
  if(b){b.classList.add('open')}
  // Highlight active row
  document.querySelectorAll('.chunk-row.active').forEach(function(r){r.classList.remove('active')});
  if(triggerEl){triggerEl.classList.add('active')}
}
function closeDrawer(){
  var d=document.getElementById('chunk-drawer');
  var b=document.getElementById('chunk-drawer-backdrop');
  if(d){d.classList.remove('open');d.setAttribute('aria-hidden','true')}
  if(b){b.classList.remove('open')}
  document.querySelectorAll('.chunk-row.active').forEach(function(r){r.classList.remove('active')});
}
document.addEventListener('keydown',function(e){if(e.key==='Escape'){closeDrawer()}});
`

func (s *Server) renderProjectList(projects []store.Project) string {
	sb := "<div id='project-list' class='project-list'>"
	if len(projects) == 0 {
		sb += "<p class='muted small'>No projects yet.</p>"
	} else {
		for _, p := range projects {
			sb += fmt.Sprintf(
				"<a href='/projects/%d' class='project-item' hx-get='/ui/projects/%d' hx-target='#main' hx-push-url='true'><span class='proj-name'>%s</span><span class='proj-domains'>%s</span></a>",
				p.ID, p.ID, template.HTMLEscapeString(p.Name), template.HTMLEscapeString(p.Domains),
			)
		}
	}
	sb += "</div>"
	return sb
}

// renderProjectDetail builds the full project panel. The jobPartial argument
// is the pre-rendered #index-stats div (active job, terminal job, or empty
// placeholder) so a page refresh during indexing resumes live polling.
func (s *Server) renderProjectDetail(p store.Project, jobPartial string) string {
	pname := template.HTMLEscapeString(p.Name)
	sb := "<div id='main' class='main project-page'>"

	// === Sticky compact header ===
	sb += "<header class='project-header'>"
	sb += "<div class='project-header-row'>"
	sb += "<div class='project-header-meta'>"
	sb += fmt.Sprintf("<h2>%s</h2>", pname)
	sb += fmt.Sprintf("<code class='project-path'>%s</code>", template.HTMLEscapeString(p.RootPath))
	if p.MachineName != "" {
		sb += fmt.Sprintf("<span class='machine-badge' title='%s on %s'>%s</span>",
			template.HTMLEscapeString(p.MachineID),
			template.HTMLEscapeString(p.MachineOS),
			template.HTMLEscapeString(p.MachineName))
	}
	sb += "</div>"
	sb += "<div class='project-header-actions'>"
	sb += "<form hx-post='/ui/index' hx-target='#index-stats' hx-swap='outerHTML'>"
	sb += fmt.Sprintf("<input type='hidden' name='project' value='%s'>", pname)
	sb += "<button type='submit' class='btn'>Re-index</button>"
	sb += "</form>"
	sb += "</div></div>"
	// Live progress bar (auto-polls while indexing, hidden when idle)
	sb += fmt.Sprintf("<div id='index-progress' hx-get='/ui/index/progress?project=%s' hx-trigger='load, every 3s' hx-swap='innerHTML'></div>", template.HTMLEscapeString(pname))
	if p.Description != "" {
		sb += fmt.Sprintf("<p class='project-desc'>%s</p>", template.HTMLEscapeString(p.Description))
	}
	// Index job status (compact when terminal, full when running)
	sb += jobPartial
	sb += "</header>"

	// === Inline search bar ===
	sb += "<div class='project-search'>"
	sb += "<form hx-get='/ui/search' hx-target='#results' class='search-form'>"
	sb += fmt.Sprintf("<input name='q' placeholder='Search %s with natural language...' type='text'>", pname)
	sb += fmt.Sprintf("<input type='hidden' name='project' value='%s'>", pname)
	sb += "<button type='submit'>Search</button></form>"
	sb += "<div id='results' class='search-results'></div>"
	sb += "</div>"

	// === Chunks browser (inline, full-width) ===
	sb += fmt.Sprintf("<section class='chunks-section' hx-get='/ui/projects/%d/chunks' hx-trigger='load' hx-swap='innerHTML'>", p.ID)
	sb += "<p class='muted small' style='padding:24px;text-align:center'>Loading indexed chunks...</p>"
	sb += "</section>"

	// === Drawer mount point ===
	sb += "<div id='chunk-drawer' class='chunk-drawer' aria-hidden='true'></div>"

	sb += "</div>"
	return sb
}

func (s *Server) renderResults(query string, results []rag.Result) string {
	// Search mode badge (hybrid or dense-only)
	modeBadge := "<span class='badge'>dense</span>"
	if s.hybridEnabled() {
		modeBadge = "<span class='badge accent'>hybrid</span>"
	}
	sb := fmt.Sprintf("<h3>Results for &ldquo;%s&rdquo; %s</h3>", template.HTMLEscapeString(query), modeBadge)
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
//
// Design system: "Neuron" — a code intelligence tool aesthetic.
// Palette: deep navy base, warm amber accent (not the typical green/blue dev tool),
// monospace-forward typography, tight information density with generous whitespace
// between sections. Signature: the amber "pulse" dot on active elements.
const cssShell = `
:root{
--bg-base:#0a0e14;--bg-surface:#111820;--bg-raised:#1a2332;--bg-overlay:#0d1219;
--border:#1e2a3a;--border-subtle:#162030;--border-focus:#d4a04840;
--text-primary:#e6edf3;--text-secondary:#8b9eb0;--text-tertiary:#5a6f82;
--accent:#d4a048;--accent-dim:#d4a04820;--accent-glow:#d4a04840;
--success:#4ade80;--success-dim:#4ade8018;
--danger:#ef4444;--danger-dim:#ef444418;
--warning:#f59e0b;
--info:#60a5fa;--info-dim:#60a5fa18;
--mono:'JetBrains Mono','Cascadia Code','Fira Code',Consolas,monospace;
--sans:'Inter',-apple-system,system-ui,sans-serif;
--radius:4px;--radius-lg:8px;
--shadow:0 2px 8px rgba(0,0,0,.3),0 1px 2px rgba(0,0,0,.2);
--shadow-lg:0 8px 32px rgba(0,0,0,.4);
}
*{box-sizing:border-box;margin:0;padding:0}
html{font-size:13px}
body{font-family:var(--sans);background:var(--bg-base);color:var(--text-primary);line-height:1.5;-webkit-font-smoothing:antialiased}
::selection{background:var(--accent-glow);color:var(--text-primary)}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:var(--radius)}

/* === Layout === */
.layout{display:grid;grid-template-columns:260px 1fr;height:100vh;overflow:hidden}
.sidebar{background:var(--bg-surface);border-right:1px solid var(--border);display:flex;flex-direction:column;overflow:hidden}
.sidebar-head{padding:20px 16px 12px;border-bottom:1px solid var(--border)}
.sidebar-brand{display:flex;align-items:center;gap:10px;margin-bottom:0}
.sidebar-brand svg{width:22px;height:22px;color:var(--accent);flex-shrink:0}
.sidebar-brand h1{font-size:14px;font-weight:600;letter-spacing:-.02em;color:var(--text-primary)}
.sidebar-brand span{color:var(--accent)}
.sidebar-scroll{flex:1;overflow-y:auto;padding:12px 0}
.sidebar-section{padding:0 12px;margin-bottom:16px}
.sidebar-section-label{font-size:10px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--text-tertiary);padding:0 4px;margin-bottom:6px}

/* === Navigation === */
.sidebar-nav{padding:0 12px;margin-bottom:8px}
.nav-link{display:flex;align-items:center;gap:8px;padding:7px 10px;border-radius:var(--radius);color:var(--text-secondary);text-decoration:none;font-size:12px;font-weight:500;cursor:pointer;transition:all 120ms ease}
.nav-link:hover{background:var(--bg-raised);color:var(--text-primary)}
.nav-link .nav-icon{font-size:14px;width:18px;text-align:center}

/* === Health Widget === */
.health-widget{background:var(--bg-base);border:1px solid var(--border);border-radius:var(--radius-lg);padding:10px 12px;margin:0 12px 12px}
.health-widget h3{display:none}
.health-row{display:flex;align-items:center;gap:8px;padding:3px 0;font-size:11px;color:var(--text-secondary)}
.health-dot{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.dot-green{background:var(--success);box-shadow:0 0 4px var(--success)}
.dot-yellow{background:var(--warning);box-shadow:0 0 4px var(--warning)}
.dot-red{background:var(--danger);box-shadow:0 0 4px var(--danger)}
.health-model{margin-top:6px;padding-top:6px;border-top:1px solid var(--border-subtle);font-size:11px;color:var(--text-tertiary);line-height:1.7}
.health-model span{color:var(--text-secondary)}

/* === Project List === */
.project-list{padding:0 4px}
.project-item{display:flex;align-items:center;justify-content:space-between;padding:8px 10px;border-radius:var(--radius);cursor:pointer;transition:all 100ms ease;border:1px solid transparent}
.project-item:hover{background:var(--bg-raised);border-color:var(--border)}
.proj-name{font-size:12px;font-weight:500;color:var(--text-primary);font-family:var(--mono)}
.proj-domains{font-size:10px;color:var(--text-tertiary);background:var(--bg-base);padding:2px 6px;border-radius:3px;font-family:var(--mono)}

/* === Add Form === */
.add-form{padding:0 12px;margin-top:auto;padding-top:12px;padding-bottom:16px;border-top:1px solid var(--border)}
.add-form summary{cursor:pointer;color:var(--accent);font-size:11px;font-weight:500;letter-spacing:.02em;padding:4px 0}
.form{display:flex;flex-direction:column;gap:6px;margin-top:8px}
.form input,.form select{padding:7px 10px;background:var(--bg-base);border:1px solid var(--border);border-radius:var(--radius);color:var(--text-primary);font-size:12px;font-family:var(--sans);transition:border-color 120ms}
.form input:focus,.form select:focus{border-color:var(--accent);outline:none;box-shadow:0 0 0 2px var(--accent-dim)}
.form input::placeholder{color:var(--text-tertiary)}
.form button,.btn{padding:7px 14px;background:var(--accent);border:none;border-radius:var(--radius);color:var(--bg-base);font-size:12px;font-weight:600;cursor:pointer;transition:all 120ms}
.form button:hover,.btn:hover{filter:brightness(1.1);transform:translateY(-1px)}
.form button:active,.btn:active{transform:translateY(0)}

/* === Main Content === */
.main{flex:1;padding:28px 32px;overflow-y:auto;background:var(--bg-base)}
.main h2{font-size:18px;font-weight:600;letter-spacing:-.02em;margin-bottom:4px}
.main h3{font-size:15px;font-weight:600;margin-bottom:8px}

/* === Search === */
.search-bar{margin-bottom:24px}
.search-form{display:flex;gap:6px;align-items:stretch}
.search-form input[name=q],.search-form input[type=text]{flex:1;padding:10px 14px;background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius);color:var(--text-primary);font-size:13px;font-family:var(--mono);transition:border-color 120ms}
.search-form input:focus{border-color:var(--accent);outline:none;box-shadow:0 0 0 2px var(--accent-dim)}
.search-form input::placeholder{color:var(--text-tertiary);font-family:var(--sans)}
.search-form select{padding:10px 12px;background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius);color:var(--text-secondary);font-size:12px;cursor:pointer}
.search-form button{padding:10px 18px;background:var(--accent);border:none;border-radius:var(--radius);color:var(--bg-base);font-weight:600;font-size:12px;cursor:pointer;white-space:nowrap}
.search-form button:hover{filter:brightness(1.1)}

/* === Results === */
.result-item{background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px 16px;margin-bottom:10px;transition:border-color 120ms}
.result-item:hover{border-color:var(--border-focus)}
.result-meta{display:flex;justify-content:space-between;align-items:center;margin-bottom:8px}
.result-file{font-family:var(--mono);font-size:12px;color:var(--accent);font-weight:500}
.result-score{font-size:11px;color:var(--text-tertiary);background:var(--bg-base);padding:2px 8px;border-radius:3px;font-family:var(--mono)}
.result-content{font-family:var(--mono);font-size:11.5px;line-height:1.6;white-space:pre-wrap;background:var(--bg-base);padding:12px 14px;border-radius:var(--radius);overflow-x:auto;color:var(--text-secondary);border:1px solid var(--border-subtle)}

/* === Utility === */
.muted{color:var(--text-secondary)}
.small{font-size:11px}
.error{color:var(--danger)}
.badge{display:inline-block;background:var(--bg-raised);color:var(--text-secondary);padding:3px 8px;border-radius:3px;font-size:10px;font-family:var(--mono);font-weight:500;letter-spacing:.02em}
.ghost-btn{padding:7px 12px;background:transparent;border:1px solid var(--border);border-radius:var(--radius);color:var(--text-secondary);cursor:pointer;font-size:12px;font-weight:500;transition:all 120ms}
.ghost-btn:hover{background:var(--bg-raised);border-color:var(--accent);color:var(--text-primary)}
.btn{padding:8px 14px;background:var(--accent);border:1px solid var(--accent);border-radius:var(--radius);color:var(--bg-base);cursor:pointer;font-size:12px;font-weight:600;transition:all 120ms}
.btn:hover{filter:brightness(1.1)}
.btn:disabled{opacity:.45;cursor:not-allowed}

/* === Project Detail === */
.project-info{margin-bottom:24px;padding-bottom:20px;border-bottom:1px solid var(--border)}
.project-info h2{color:var(--text-primary);margin-bottom:2px}
.project-info p{color:var(--text-secondary);font-size:12px;margin-bottom:4px}

/* === Index Stats === */
.index-stats{background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px 16px;margin-top:12px}
.index-stats h3{font-size:13px;font-weight:600;margin-bottom:6px}
.index-stats pre{font-family:var(--mono);font-size:11px;color:var(--text-secondary);white-space:pre-wrap}
.index-stats table{font-size:11px;font-family:var(--mono)}
.index-stats table td{padding:2px 0}

/* === HTMX === */
.htmx-indicator{opacity:0;transition:opacity 200ms ease-in}
.htmx-request .htmx-indicator,.htmx-request.htmx-indicator{opacity:1}

/* === Chunks Browser === */
.chunks-main{padding:20px 24px}
.chunks-topbar{display:flex;align-items:center;gap:14px;margin-bottom:16px;padding-bottom:12px;border-bottom:1px solid var(--border)}
.chunks-topbar h2{font-size:17px;letter-spacing:-.01em;color:var(--text-primary)}
.chunks-grid{display:grid;grid-template-columns:minmax(480px,3fr) minmax(320px,2fr);gap:14px;min-height:calc(100vh - 120px)}
.chunks-left{min-width:0;display:flex;flex-direction:column;gap:10px}
.chunk-filters{position:sticky;top:0;z-index:5;background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:12px;box-shadow:var(--shadow)}
.filter-row{display:grid;grid-template-columns:1.3fr 1fr 1fr .75fr .75fr;gap:8px;align-items:end;margin-top:8px}
.filter-row.primary{grid-template-columns:auto 1fr auto auto;margin-top:0}
.filter-row label{display:flex;flex-direction:column;gap:4px;color:var(--text-tertiary);font-size:10px;text-transform:uppercase;letter-spacing:.06em;font-weight:500}
.filter-row input,.filter-row select,.chunk-search-input{padding:7px 10px;background:var(--bg-base);border:1px solid var(--border);border-radius:var(--radius);color:var(--text-primary);font-size:12px;min-width:0}
.filter-row input:focus,.filter-row select:focus,.chunk-search-input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 2px var(--accent-dim)}
.filter-row button,.chunk-pager button{padding:7px 12px;background:var(--accent);border:0;border-radius:var(--radius);color:var(--bg-base);cursor:pointer;font-size:12px;font-weight:600}
.filter-row button:disabled,.chunk-pager button:disabled{opacity:.4;cursor:not-allowed;background:var(--bg-raised);color:var(--text-tertiary)}
.mode-toggle{display:flex;border:1px solid var(--border);border-radius:var(--radius);overflow:hidden;background:var(--bg-base)}
.mode-toggle label{display:block;color:var(--text-tertiary);font-size:11px;text-transform:none;letter-spacing:0;cursor:pointer}
.mode-toggle input{display:none}
.mode-toggle span{display:block;padding:7px 12px}
.mode-toggle input:checked+span{background:var(--accent);color:var(--bg-base);font-weight:600}
.advanced-filters summary{cursor:pointer;color:var(--text-tertiary);font-size:11px;margin-top:8px}
.chunk-table-card{border:1px solid var(--border);border-radius:var(--radius-lg);overflow:hidden;background:var(--bg-surface);min-height:400px}
.chunk-table-status{display:flex;justify-content:space-between;align-items:center;padding:8px 12px;border-bottom:1px solid var(--border);background:var(--bg-raised);font-size:11px;color:var(--text-tertiary)}
.chunk-table{width:100%;border-collapse:separate;border-spacing:0;font-size:11px}
.chunk-table thead th{position:sticky;top:0;background:var(--bg-surface);color:var(--text-tertiary);text-align:left;font-weight:600;padding:8px 10px;border-bottom:1px solid var(--border);z-index:2;font-size:10px;text-transform:uppercase;letter-spacing:.04em}
.chunk-table tbody td{padding:8px 10px;border-bottom:1px solid var(--border-subtle);vertical-align:top}
.chunk-row{cursor:pointer;transition:background 80ms ease}
.chunk-row:hover{background:var(--bg-raised)}
.chunk-file{font-family:var(--mono);color:var(--accent);font-size:11px;margin-bottom:3px;word-break:break-all}
.chunk-preview{color:var(--text-tertiary);line-height:1.4;max-width:480px;font-size:11px}
.line-chip,.symbol-chip,.score-pill{display:inline-block;border-radius:3px;padding:2px 6px;font-size:10px;border:1px solid var(--border);background:var(--bg-base);color:var(--text-secondary);white-space:nowrap;font-family:var(--mono)}
.symbol-chip{color:#c4b5fd;border-color:#3b2d52;background:#1a1028}
.chunk-name{margin-top:3px;color:var(--text-tertiary);max-width:160px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;font-size:11px}
.score-mode{color:var(--success);background:var(--success-dim);border-color:var(--success)}
.score-high{color:var(--success);border-color:var(--success);background:var(--success-dim)}
.score-mid{color:var(--warning);border-color:var(--warning);background:#f59e0b18}
.score-low{color:var(--text-tertiary);border-color:var(--border);background:var(--bg-base)}
.chunk-pager{display:flex;justify-content:space-between;align-items:center;padding:10px 12px;border-top:1px solid var(--border);color:var(--text-tertiary);font-size:11px}
.chunk-detail{border:1px solid var(--border);border-radius:var(--radius-lg);background:var(--bg-surface);min-width:0;overflow:auto;position:sticky;top:16px;max-height:calc(100vh - 120px)}
.chunk-empty{height:100%;display:flex;align-items:center;justify-content:center;text-align:center;flex-direction:column;padding:28px;color:var(--text-tertiary)}
.chunk-empty.compact{min-height:220px}
.chunk-empty-mark{font-size:36px;color:var(--border);margin-bottom:8px}
.chunk-detail-card{padding:14px 16px}
.chunk-detail-head{display:flex;justify-content:space-between;gap:10px;align-items:flex-start;margin-bottom:10px;padding-bottom:10px;border-bottom:1px solid var(--border)}
.chunk-detail-head h3{font-size:14px;color:var(--text-primary);word-break:break-word;font-family:var(--mono);font-weight:500}
.detail-badges{display:flex;gap:4px;flex-wrap:wrap;justify-content:flex-end}
.chunk-code{font-family:var(--mono);font-size:11.5px;line-height:1.6;white-space:pre;overflow:auto;background:var(--bg-base);border:1px solid var(--border-subtle);border-radius:var(--radius);padding:12px 14px;color:var(--text-secondary);max-height:55vh}
.chunk-meta{margin-top:10px;border-top:1px solid var(--border);padding-top:8px;display:grid;gap:4px;font-size:11px}
.chunk-meta div{display:grid;grid-template-columns:80px 1fr;gap:8px;color:var(--text-tertiary)}
.chunk-meta code{font-family:var(--mono);color:var(--text-secondary);word-break:break-all}

/* === Models === */
.models-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:10px;margin-top:12px}
.model-card{background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px 16px;transition:all 120ms}
.model-card:hover{border-color:var(--accent);box-shadow:0 0 0 1px var(--accent-dim)}
.model-card.model-active{border-color:var(--accent);background:var(--accent-dim)}
.model-card-head{display:flex;justify-content:space-between;align-items:center;margin-bottom:8px}
.model-card-head h3{font-size:13px;font-weight:600;color:var(--text-primary);font-family:var(--mono)}
.model-meta{display:flex;gap:6px;flex-wrap:wrap;font-size:10px;color:var(--text-tertiary);font-family:var(--mono)}
.model-meta span{background:var(--bg-base);padding:2px 6px;border-radius:3px;border:1px solid var(--border-subtle)}
.model-meta .meta-chip.mono{font-family:var(--mono)}
.model-endpoint{margin-top:6px;font-size:10px;color:var(--text-tertiary);word-break:break-all}
.model-actions{margin-top:10px;display:flex;gap:6px;align-items:center}
.models-header{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:12px;flex-wrap:wrap}
.models-header h2{margin:0 0 4px 0}
.badge.accent{background:var(--accent-dim);color:var(--accent);border:1px solid var(--accent)}
.badge.danger{background:var(--danger-dim,#3a1a1a);color:var(--danger,#ff5c5c);border:1px solid var(--danger,#ff5c5c)}
.ghost-btn.danger{color:var(--danger,#ff5c5c)}
.ghost-btn.danger:hover{background:var(--danger-dim,#3a1a1a)}

/* === Model form wizard === */
.model-form-wrapper{max-width:560px;margin-top:16px}
.model-form-head{display:flex;justify-content:space-between;align-items:center;margin-bottom:14px}
.model-form-head h3{font-size:14px;font-weight:600}
.model-form{display:flex;flex-direction:column;gap:14px}
.form-section{display:flex;flex-direction:column;gap:6px}
.form-section .form-row{display:flex;gap:8px;align-items:center}
.form-section .form-row .form-input{flex:1}
.form-grid-2{display:grid;grid-template-columns:1fr 1fr;gap:10px}
.form-label{font-size:11px;color:var(--text-secondary);font-weight:500;letter-spacing:.02em;text-transform:uppercase}
.form-input{background:var(--bg-base);border:1px solid var(--border);color:var(--text-primary);padding:8px 10px;border-radius:var(--radius);font-size:12px;font-family:var(--sans);width:100%}
.form-input.mono{font-family:var(--mono)}
.form-input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 2px var(--accent-dim)}
.form-readonly{background:var(--bg-base);border:1px solid var(--border-subtle);padding:8px 10px;border-radius:var(--radius);font-size:12px;color:var(--text-secondary)}
.form-note{font-size:10px;color:var(--text-tertiary);margin-top:2px}
.form-note.warn{color:var(--accent)}
.form-advanced{margin-top:6px;background:var(--bg-surface);border:1px solid var(--border-subtle);border-radius:var(--radius);padding:0}
.form-advanced summary{cursor:pointer;padding:10px 12px;font-size:11px;font-weight:500;color:var(--text-secondary);text-transform:uppercase;letter-spacing:.04em;user-select:none}
.form-advanced summary:hover{color:var(--text-primary)}
.form-advanced[open]{padding:0 12px 12px 12px}
.form-advanced[open] summary{padding-left:0;padding-right:0;margin-bottom:8px;border-bottom:1px solid var(--border-subtle)}
.form-advanced .form-section{margin-top:10px}
.form-actions{display:flex;gap:8px;margin-top:8px}

/* === Settings panel === */
.settings-panel h2{margin-bottom:4px}
.settings-card{background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:18px 20px}
.settings-card h3{font-size:13px;font-weight:600;margin-bottom:12px}
.settings-row{display:grid;grid-template-columns:160px 1fr;gap:16px;padding:10px 0;align-items:start}
.settings-row-label{font-size:11px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:.04em;padding-top:4px}
.settings-row-value{font-size:12px;color:var(--text-primary)}
.banner.success{background:rgba(67,160,71,.08);border-color:rgba(67,160,71,.4) !important;color:#7ed084}
.banner.error{background:rgba(255,92,92,.08);border-color:rgba(255,92,92,.4) !important;color:#ff8c8c}
.banner.warn{background:rgba(255,193,7,.08);border-color:rgba(255,193,7,.4) !important;color:#ffd54a}
.switch-steps li{margin:2px 0}

/* === Compare === */
.compare-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px;margin-top:14px}
.compare-col{background:var(--bg-surface);border:1px solid var(--border);border-radius:var(--radius-lg);padding:14px}
.compare-col h4{font-size:13px;font-weight:600;color:var(--text-primary);margin-bottom:4px;font-family:var(--mono)}
.compare-result{display:flex;gap:8px;align-items:center;padding:4px 0;font-size:11px;border-bottom:1px solid var(--border-subtle)}
.compare-result:last-child{border-bottom:none}

/* === Responsive === */
@media(max-width:1100px){
.layout{grid-template-columns:1fr}
.sidebar{display:none}
.chunks-grid{grid-template-columns:1fr}
.chunk-detail{position:relative;top:0;max-height:none}
.filter-row,.filter-row.primary{grid-template-columns:1fr}
.mode-toggle{width:max-content}
}

/* === Scrollbar === */
::-webkit-scrollbar{width:6px;height:6px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:var(--border);border-radius:3px}
::-webkit-scrollbar-thumb:hover{background:var(--text-tertiary)}

/* === Project Page (inline chunks) === */
.project-page{padding:0;max-width:none}
.project-header{position:sticky;top:0;z-index:20;background:rgba(10,14,20,.92);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);border-bottom:1px solid var(--border);padding:18px 28px 14px}
.project-header-row{display:flex;align-items:center;gap:16px;justify-content:space-between;flex-wrap:wrap}
.project-header-meta{min-width:0;flex:1;display:flex;align-items:baseline;gap:12px;flex-wrap:wrap}
.project-header-meta h2{font-size:18px;font-weight:600;letter-spacing:-.02em;color:var(--text-primary);margin:0}
.project-path{font-family:var(--mono);font-size:11.5px;color:var(--text-tertiary);background:var(--bg-surface);padding:3px 8px;border-radius:3px;border:1px solid var(--border-subtle);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:60ch;display:inline-block}
.machine-badge{font-family:var(--mono);font-size:10.5px;color:var(--accent-primary);background:var(--bg-elevated);padding:2px 7px;border-radius:3px;border:1px solid var(--accent-primary);margin-left:6px;white-space:nowrap;cursor:help}
.project-header-actions{display:flex;gap:8px;flex-shrink:0}
.project-desc{color:var(--text-secondary);font-size:12px;margin-top:6px}
.index-progress{margin:12px 0;padding:12px 16px;background:var(--bg-surface);border-radius:8px;border:1px solid var(--border-subtle)}
.progress-bar-wrap{height:6px;background:var(--bg-raised);border-radius:3px;overflow:hidden;margin-bottom:6px}
.progress-bar-fill{height:100%;background:var(--accent-primary);border-radius:3px;transition:width 0.3s ease}
.progress-label{font-size:12px;color:var(--text-secondary)}
.progress-file{font-size:11px;color:var(--text-tertiary);margin-top:2px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.index-info-card{background:var(--bg-surface);border:1px solid var(--border-subtle);border-radius:8px;padding:16px 20px;margin:8px 0}
.index-info-card .info-icon{font-size:24px;color:var(--accent-primary);margin:0 0 8px 0}
.index-info-card p{margin:4px 0;line-height:1.5}
.index-info-card .muted{color:var(--text-secondary)}
.index-info-card code{font-family:var(--mono);font-size:11.5px;background:var(--bg-raised);padding:2px 6px;border-radius:3px;border:1px solid var(--border-subtle);color:var(--accent-primary)}
.index-info-card .small{font-size:11.5px}
.project-search{padding:16px 28px 0}
.search-results{margin-top:14px}
.search-results:empty{display:none}

/* index-stats inside header gets compact treatment */
.project-header .index-stats{margin-top:10px;padding:8px 12px;background:var(--bg-surface);border-radius:var(--radius);font-size:11.5px}
.project-header .index-stats h3{font-size:12px;margin:0 0 4px;display:inline-block;margin-right:8px}
.project-header .index-stats h3+p,.project-header .index-stats h3+table{display:inline-block;vertical-align:middle}
.project-header .index-stats table{font-size:11px}
.project-header .index-stats table td{padding:0 12px 0 0;display:inline-block}
.project-header .index-stats table tr{display:inline}

/* === Chunks section (inline, no aside) === */
.chunks-section{padding:8px 28px 28px}
.chunks-section .chunks-topbar{display:none}  /* no back-button needed now */
.chunks-section .chunks-grid{display:block;min-height:0}
.chunks-section .chunks-left{gap:10px}
.chunks-section .chunk-detail{display:none}  /* drawer takes over */

/* Override chunks-main for inline mode */
.project-page .chunks-main{padding:0;background:transparent}

/* === Drawer === */
.chunk-drawer{position:fixed;top:0;right:0;height:100vh;width:min(560px,90vw);background:var(--bg-surface);border-left:1px solid var(--border);box-shadow:-12px 0 32px rgba(0,0,0,.5);transform:translateX(100%);transition:transform 220ms cubic-bezier(.22,.61,.36,1);z-index:50;overflow-y:auto;display:flex;flex-direction:column}
.chunk-drawer.open{transform:translateX(0)}
.chunk-drawer-backdrop{position:fixed;inset:0;background:rgba(0,0,0,.4);opacity:0;transition:opacity 220ms;pointer-events:none;z-index:40}
.chunk-drawer.open ~ .chunk-drawer-backdrop,.chunk-drawer-backdrop.open{opacity:1;pointer-events:auto}
.chunk-drawer-head{display:flex;align-items:center;justify-content:space-between;padding:14px 18px;border-bottom:1px solid var(--border);position:sticky;top:0;background:var(--bg-surface);z-index:2}
.chunk-drawer-head h3{font-size:13px;font-weight:600;color:var(--text-primary);font-family:var(--mono)}
.chunk-drawer-close{background:transparent;border:0;color:var(--text-tertiary);cursor:pointer;font-size:18px;padding:4px 8px;border-radius:var(--radius);line-height:1}
.chunk-drawer-close:hover{background:var(--bg-raised);color:var(--text-primary)}
.chunk-drawer-body{padding:16px 18px;overflow-y:auto}

/* highlight active row */
.chunk-row.active{background:var(--accent-dim);box-shadow:inset 3px 0 0 var(--accent)}

/* === Animations === */
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.5}}
.dot-green{animation:pulse 3s ease-in-out infinite}
`
