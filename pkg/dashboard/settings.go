package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	"github.com/labstack/echo/v4"
)

// uiSettingsPanel renders the Settings page (currently: GPU runtime card).
func (s *Server) uiSettingsPanel(c echo.Context) error {
	st := detectGPU(c.Request().Context())
	st.CurrentMode = s.store.GetSetting(c.Request().Context(), "embedder_mode")
	if st.CurrentMode == "" {
		st.CurrentMode = "cpu"
	}
	st.EmbedderImage, st.EmbedderStatus = embedderContainerInfo(c.Request().Context())

	var sb strings.Builder
	sb.WriteString("<div id='main' class='main settings-panel'>")
	sb.WriteString("<h2>Settings</h2>")
	sb.WriteString("<p class='muted small'>Runtime, hardware, and provider defaults.</p>")

	// Health issues banner (above the runtime card so it's the first thing users see)
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
	}
	sb.WriteString("</div></div>")

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

	sb.WriteString("</section>")
	sb.WriteString("</div>")
	return c.HTML(200, sb.String())
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
