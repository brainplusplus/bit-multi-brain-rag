package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// uiSettingsPanel renders the Settings page as HTMX partial.
func (s *Server) uiSettingsPanel(c echo.Context) error {
	return c.HTML(200, s.buildSettingsHTML(c))
}

// buildSettingsHTML returns the settings panel HTML string.
// Shared between HTMX partial (uiSettingsPanel) and full-page route (uiSettingsPage).
func (s *Server) buildSettingsHTML(c echo.Context) string {
	// Check if running in embedded (zvec) mode
	embeddedMode := s.cfg.ZvecPath != ""

	st := detectGPU(c.Request().Context())
	st.CurrentMode = s.store.GetSetting(c.Request().Context(), "embedder_mode")
	if st.CurrentMode == "" {
		if embeddedMode {
			st.CurrentMode = "local"
		} else {
			st.CurrentMode = "cpu"
		}
	}
	if !embeddedMode {
		st.EmbedderImage, st.EmbedderStatus = embedderContainerInfo(c.Request().Context())
	}

	var sb strings.Builder
	sb.WriteString("<div id='main' class='main settings-panel'>")
	sb.WriteString("<h2>Settings</h2>")
	sb.WriteString("<p class='muted small'>Runtime, hardware, and provider defaults.</p>")

	// Health issues banner (skip Docker-specific issues in embedded mode)
	if !embeddedMode {
		for _, issue := range st.HealthIssues() {
		cls := "banner error"
		icon := "⚠"
		if issue.Severity == "warn" {
			cls = "banner warn"
			icon = "ℹ"
		}
		sb.WriteString(fmt.Sprintf("<div class='%s' style='margin-top:16px;padding:12px 14px;border-radius:8px;border:1px solid var(--border)'>", cls))
		sb.WriteString(fmt.Sprintf("<strong>%s %s</strong>", icon, template.HTMLEscapeString(issue.Title)))
		sb.WriteString(fmt.Sprintf("<div class='small' style='margin-top:4px'>%s</div>", template.HTMLEscapeString(issue.Detail)))
		if issue.Fix != "" {
			sb.WriteString(fmt.Sprintf("<div class='mono small' style='margin-top:8px;padding:8px 10px;background:rgba(0,0,0,0.25);border-radius:6px;user-select:all;white-space:pre-wrap;word-break:break-all'>%s</div>",
				template.HTMLEscapeString(issue.Fix)))
		}
		sb.WriteString("</div>")
	}
	} // end if !embeddedMode

	sb.WriteString("<section class='settings-card' style='margin-top:24px'>")
	sb.WriteString("<h3>Embedder runtime</h3>")

	// Current mode badge
	modeBadge := "<span class='badge'>CPU mode</span>"
	if st.CurrentMode == "gpu" {
		modeBadge = "<span class='badge accent'>GPU mode</span>"
	}
	sb.WriteString("<div class='settings-row'>")
	sb.WriteString("<div class='settings-row-label'>Current mode</div>")
	sb.WriteString("<div class='settings-row-value'>" + modeBadge)
	if st.EmbedderImage != "" {
		sb.WriteString(fmt.Sprintf("<span class='muted small mono' style='margin-left:8px'>%s · %s</span>",
			template.HTMLEscapeString(st.EmbedderImage), template.HTMLEscapeString(st.EmbedderStatus)))
	}
	sb.WriteString("</div></div>")

	// GPU detection
	sb.WriteString("<div class='settings-row'>")
	sb.WriteString("<div class='settings-row-label'>NVIDIA GPU</div>")
	sb.WriteString("<div class='settings-row-value'>")
	if !st.Detected {
		sb.WriteString("<span class='badge danger'>not detected</span>")
		sb.WriteString("<div class='muted small' style='margin-top:6px'>Install NVIDIA drivers + Container Toolkit, then mount /proc/driver/nvidia into this container to enable GPU mode.</div>")
	} else {
		sb.WriteString(fmt.Sprintf("<span class='badge accent'>%s</span>", template.HTMLEscapeString(st.Name)))
		sb.WriteString("<div class='muted small mono' style='margin-top:6px'>")
		if st.VRAMTotalMB > 0 {
			sb.WriteString(fmt.Sprintf("VRAM: %d / %d MB · ", st.VRAMUsedMB, st.VRAMTotalMB))
		}
		if st.DriverVersion != "" {
			sb.WriteString("driver " + template.HTMLEscapeString(st.DriverVersion))
		}
		sb.WriteString("</div>")
		// Note when detected via CDI inference (no direct nvidia-smi access)
		if st.VRAMTotalMB == 0 && st.DriverVersion == "" && st.CDIDevices > 0 {
			sb.WriteString("<div class='muted small' style='margin-top:6px'>Detected via Docker CDI. Full GPU details will appear once the embedder runs in GPU mode.</div>")
		}
	}
	sb.WriteString("</div></div>")

	if embeddedMode {
		// Embedded mode: show local embedder controls
		sb.WriteString("<div class='settings-row'>")
		sb.WriteString("<div class='settings-row-label'>Storage</div>")
		sb.WriteString("<div class='settings-row-value'><span class='badge accent'>zvec embedded</span> <span class='muted small'>in-process, no Docker</span></div>")
		sb.WriteString("</div>")

		sb.WriteString("<div class='settings-row'>")
		sb.WriteString("<div class='settings-row-label'>Embedder</div>")
		sb.WriteString("<div class='settings-row-value'><span class='badge accent'>local binary</span> <span class='muted small'>" + template.HTMLEscapeString(s.cfg.EmbeddingEndpoint) + "</span></div>")
		sb.WriteString("</div>")

		// GPU switch for local embedder (only if embedder binary is managed by dashboard)
		if s.embedderMgr != nil {
			sb.WriteString("<div class='settings-row' style='border-top:1px solid var(--border);padding-top:16px;margin-top:16px'>")
			sb.WriteString("<div class='settings-row-label'>Switch runtime</div>")
			sb.WriteString("<div class='settings-row-value'>")
			if st.CurrentMode == "gpu" {
				sb.WriteString("<button class='btn' hx-post='/api/v1/embedder/switch' hx-vals='{\"mode\":\"cpu\"}' hx-target='#main' hx-swap='outerHTML' hx-confirm='Restart embedder in CPU mode?'>Switch to CPU</button>")
			} else {
				canSwitch := st.Detected
				btnAttrs := ""
				if !canSwitch {
					btnAttrs = " disabled title='GPU not detected'"
				}
				sb.WriteString(fmt.Sprintf("<button class='btn'%s hx-post='/api/v1/embedder/switch' hx-vals='{\"mode\":\"gpu\"}' hx-target='#main' hx-swap='outerHTML' hx-confirm='Restart embedder in GPU mode?'>Switch to GPU</button>", btnAttrs))
			}
			sb.WriteString("<div class='muted small' style='margin-top:8px'>Restarts the local llama-server with/without GPU acceleration.</div>")
			sb.WriteString("</div></div>")
		} else {
			sb.WriteString("<div class='muted small' style='margin-top:16px;padding-top:16px;border-top:1px solid var(--border)'>Set <code>EMBEDDER_BINARY</code> to enable GPU/CPU switching for the local embedder.</div>")
		}
	} else {
		// Docker mode: show container toolkit + switch buttons
		// Container toolkit
		sb.WriteString("<div class='settings-row'>")
		sb.WriteString("<div class='settings-row-label'>Container toolkit</div>")
	sb.WriteString("<div class='settings-row-value'>")
	if st.ContainerToolkit {
		sb.WriteString("<span class='badge accent'>nvidia-container-runtime ready</span>")
		// Show CDI device count if known
		if st.CDIDevices > 0 {
			sb.WriteString(fmt.Sprintf(" <span class='badge accent'>%d CDI device(s)</span>", st.CDIDevices))
		} else if st.HostType == "rancher-desktop" || st.HostType == "linux" {
			sb.WriteString(" <span class='badge'>no CDI devices</span>")
		}
	} else {
		sb.WriteString("<span class='badge'>not registered</span>")
		if st.HostType == "rancher-desktop" {
			sb.WriteString("<div class='muted small' style='margin-top:6px'>Rancher Desktop detected. See the banner above for the repair command.</div>")
		} else {
			sb.WriteString("<div class='muted small' style='margin-top:6px'>Run <span class='mono'>sudo nvidia-ctk runtime configure --runtime=docker</span> on the host, then restart docker.</div>")
		}
	}
	if st.HostType != "" && st.HostType != "unknown" {
		sb.WriteString(fmt.Sprintf("<div class='muted small mono' style='margin-top:6px'>host: %s</div>", template.HTMLEscapeString(st.HostType)))
	}
	sb.WriteString("</div></div>")

	// Switch button
	sb.WriteString("<div class='settings-row' style='border-top:1px solid var(--border);padding-top:16px;margin-top:16px'>")
	sb.WriteString("<div class='settings-row-label'>Switch runtime</div>")
	sb.WriteString("<div class='settings-row-value'>")
	if st.CurrentMode == "gpu" {
		sb.WriteString("<button class='btn' hx-post='/ui/settings/gpu/switch' hx-vals='{\"mode\":\"cpu\"}' hx-target='#main' hx-swap='outerHTML' hx-confirm='Switch embedder to CPU mode? This will recreate the container and may take ~30s.'>Switch to CPU</button>")
	} else {
		canSwitch := st.Detected && st.ContainerToolkit
		btnAttrs := ""
		if !canSwitch {
			btnAttrs = " disabled title='GPU or container toolkit unavailable'"
		}
		sb.WriteString(fmt.Sprintf("<button class='btn'%s hx-post='/ui/settings/gpu/switch' hx-vals='{\"mode\":\"gpu\"}' hx-target='#main' hx-swap='outerHTML' hx-confirm='Switch embedder to GPU mode? This will pull bit-rag-embedder:gpu, stop the current container, and start the new one with auto-rollback on failure.'>Switch to GPU</button>", btnAttrs))
	}
	sb.WriteString("<div class='muted small' style='margin-top:8px'>The switch is automated: pre-flight check → pull image → stop old → start new → health probe → persist. On failure, auto-rollback to the previous image.</div>")
	sb.WriteString("</div></div>")
	} // end Docker mode

	// Search configuration section (ADR-0008: hybrid search)
	sb.WriteString("<section class='settings-card' style='margin-top:24px'>")
	sb.WriteString("<h3>Search configuration</h3>")
	sb.WriteString("<div class='settings-row'>")
	sb.WriteString("<div class='settings-row-label'>Hybrid search</div>")
	sb.WriteString("<div class='settings-row-value'>")
	hybridVal := s.store.GetSetting(c.Request().Context(), "hybrid_search")
	if hybridVal == "" {
		hybridVal = "on" // default
	}
	if hybridVal == "on" {
		sb.WriteString("<span class='badge accent'>enabled</span> <span class='muted small'>dense (voyage-4-nano) + sparse (BM25) + RRF fusion</span>")
	} else {
		sb.WriteString("<span class='badge'>disabled</span> <span class='muted small'>dense-only (semantic search)</span>")
	}
	// Toggle button
	toggleMode := "on"
	toggleLabel := "Enable"
	if hybridVal == "on" {
		toggleMode = "off"
		toggleLabel = "Disable"
	}
	sb.WriteString(fmt.Sprintf("<div style='margin-top:8px'><button class='btn btn-sm' hx-post='/ui/settings/hybrid/toggle' hx-vals='{\"mode\":\"%s\"}' hx-target='#main' hx-swap='outerHTML'>%s hybrid</button>", toggleMode, toggleLabel))
	sb.WriteString("<div class='muted small' style='margin-top:6px'>Hybrid search combines semantic (dense) and keyword (sparse/BM25) retrieval with Reciprocal Rank Fusion. Improves exact identifier matching. Requires re-indexing to take effect.</div>")
	sb.WriteString("</div>")
	sb.WriteString("</div></div>")
	sb.WriteString("</section>")

	sb.WriteString("</section>")
	sb.WriteString("</div>")
	return sb.String()
}

// uiGPUSwitch executes the switch synchronously and re-renders the Settings panel
// with the outcome banner at the top.
func (s *Server) uiGPUSwitch(c echo.Context) error {
	mode := c.FormValue("mode")
	if mode == "" {
		// HTMX may send as JSON via hx-vals
		mode = c.QueryParam("mode")
	}
	if mode != "cpu" && mode != "gpu" {
		// Try parsing JSON body
		var req switchRequest
		if err := c.Bind(&req); err == nil {
			mode = req.Mode
		}
	}
	if mode != "cpu" && mode != "gpu" {
		return c.HTML(400, "<div id='main' class='main'><p class='error'>Invalid mode.</p></div>")
	}
	if !gpuMu.TryLock() {
		return c.HTML(409, "<div id='main' class='main'><p class='error'>Another switch is in progress.</p></div>")
	}
	gpuSwitchInProgress = true
	res := s.performSwitch(c.Request().Context(), mode)
	gpuSwitchInProgress = false
	gpuMu.Unlock()

	// Render banner + settings page
	var sb strings.Builder
	sb.WriteString("<div id='main' class='main settings-panel'>")
	cls := "banner success"
	if !res.OK {
		cls = "banner error"
	}
	sb.WriteString(fmt.Sprintf("<div class='%s' style='margin-bottom:16px;padding:12px 14px;border-radius:8px;border:1px solid var(--border)'>", cls))
	sb.WriteString(fmt.Sprintf("<strong>%s</strong>", template.HTMLEscapeString(res.Message)))
	if res.Rollback {
		sb.WriteString(" <span class='muted small'>(rolled back)</span>")
	}
	sb.WriteString("<ol class='switch-steps' style='margin-top:8px;padding-left:20px;font-size:12px'>")
	for _, st := range res.Steps {
		icon := "✓"
		switch st.Status {
		case "failed":
			icon = "✗"
		case "skipped":
			icon = "—"
		}
		sb.WriteString(fmt.Sprintf("<li>%s %s <span class='muted'>(%dms)</span>",
			icon, template.HTMLEscapeString(st.Name), st.Duration))
		if st.Detail != "" {
			sb.WriteString(fmt.Sprintf(" — <span class='muted'>%s</span>", template.HTMLEscapeString(st.Detail)))
		}
		sb.WriteString("</li>")
	}
	sb.WriteString("</ol></div>")

	// Re-render full settings page below banner
	// (replay logic from uiSettingsPanel without the outer div+header)
	st := detectGPU(c.Request().Context())
	st.CurrentMode = s.store.GetSetting(c.Request().Context(), "embedder_mode")
	if st.CurrentMode == "" {
		st.CurrentMode = "cpu"
	}
	sb.WriteString(fmt.Sprintf("<p class='muted small'>Current mode: <strong>%s</strong>. <button class='ghost-btn' hx-get='/ui/settings' hx-target='#main' hx-swap='outerHTML'>Reload settings</button></p>",
		template.HTMLEscapeString(strings.ToUpper(st.CurrentMode))))
	sb.WriteString("</div>")
	return c.HTML(200, sb.String())
}

// (keep context import alive for future async switch streaming)
var _ = context.Background

// uiHybridToggle toggles hybrid search on/off and re-renders the Settings page.
func (s *Server) uiHybridToggle(c echo.Context) error {
	mode := c.FormValue("mode")
	if mode != "on" && mode != "off" {
		return c.HTML(400, "<div id='main' class='main'><p class='error'>Invalid mode. Use on or off.</p></div>")
	}
	if err := s.store.SetSetting(c.Request().Context(), "hybrid_search", mode); err != nil {
		return c.HTML(500, "<div id='main' class='main'><p class='error'>Failed to save setting: "+template.HTMLEscapeString(err.Error())+"</p></div>")
	}
	// Re-render the full settings panel.
	return s.uiSettingsPanel(c)
}

// apiEmbedderSwitch restarts the local embedder binary with GPU/CPU toggle.
// POST /api/v1/embedder/switch {"mode":"gpu"|"cpu"}
// Only works when EMBEDDER_BINARY is configured (embedded mode).
func (s *Server) apiEmbedderSwitch(c echo.Context) error {
	if s.embedderMgr == nil {
		return c.JSON(400, map[string]string{"error": "embedder binary manager not configured. Set EMBEDDER_BINARY to enable local embedder switching."})
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request"})
	}
	if req.Mode != "gpu" && req.Mode != "cpu" {
		return c.JSON(400, map[string]string{"error": "mode must be 'gpu' or 'cpu'"})
	}

	// Stop existing embedder, flip GPU flag, restart.
	s.embedderMgr.Stop()
	s.embedderMgr.SetGPU(req.Mode == "gpu")

	ctx, cancel := context.WithTimeout(c.Request().Context(), 120*time.Second)
	defer cancel()

	endpoint, err := s.embedderMgr.Start(ctx)
	if err != nil {
		return c.JSON(500, map[string]string{"error": fmt.Sprintf("restart failed: %v", err)})
	}

	// Persist mode
	s.store.SetSetting(c.Request().Context(), "embedder_mode", req.Mode)

	return c.JSON(200, map[string]any{
		"ok":       true,
		"mode":     req.Mode,
		"endpoint": endpoint,
		"message":  fmt.Sprintf("Embedder restarted in %s mode", req.Mode),
	})
}
