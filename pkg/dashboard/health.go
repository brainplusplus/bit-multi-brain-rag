package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// healthData holds the system health snapshot for rendering.
type healthData struct {
	QdrantStatus     string // "green", "yellow", "red", "offline"
	QdrantPoints     int
	QdrantCollections int
	EmbedderStatus   string // "healthy", "offline"
	EmbedderModel    string
	EmbedderBackend  string
	EmbedderDim      int
	Uptime           time.Duration
}

var startTime = time.Now()

// uiHealth renders the health widget (HTMX partial, polled every 30s).
func (s *Server) uiHealth(c echo.Context) error {
	data := s.probeHealth(c.Request().Context())
	return c.HTML(200, s.renderHealthWidget(data))
}

// apiHealth returns system health as JSON.
func (s *Server) apiHealth(c echo.Context) error {
	data := s.probeHealth(c.Request().Context())
	return c.JSON(200, map[string]any{
		"qdrant_status":      data.QdrantStatus,
		"qdrant_points":      data.QdrantPoints,
		"qdrant_collections": data.QdrantCollections,
		"embedder_status":    data.EmbedderStatus,
		"embedder_model":     data.EmbedderModel,
		"embedder_backend":   data.EmbedderBackend,
		"embedder_dim":       data.EmbedderDim,
		"uptime_seconds":     int(data.Uptime.Seconds()),
	})
}

func (s *Server) probeHealth(ctx context.Context) healthData {
	data := healthData{
		QdrantStatus:   "offline",
		EmbedderStatus: "offline",
		Uptime:         time.Since(startTime),
	}
	// Probe Qdrant.
	if s.rag != nil {
		if err := s.rag.Ping(ctx); err == nil {
			data.QdrantStatus = "green"
			// Try to get collection info for the active project.
			key := s.collectionKeyFor("bit-rag-self") // probe default
			if info, err := s.rag.CollectionInfo(ctx, key); err == nil {
				data.QdrantPoints = info.PointsCount
			}
		}
	}
	// Probe embedder.
	if s.embed != nil {
		// Quick embed of empty-ish text to check liveness.
		_, err := s.embed.Embed(ctx, []string{"health check"})
		if err == nil {
			data.EmbedderStatus = "healthy"
		}
		data.EmbedderModel = s.embed.Model()
		data.EmbedderBackend = s.embed.Backend()
		data.EmbedderDim = s.embed.VectorSize()
	}
	return data
}

func (s *Server) renderHealthWidget(data healthData) string {
	var sb strings.Builder
	sb.WriteString("<div id='health-widget' class='health-widget' hx-get='/ui/health' hx-trigger='every 30s' hx-swap='outerHTML'>")
	// Status rows
	sb.WriteString("<div class='health-row'>")
	sb.WriteString(fmt.Sprintf("<span class='health-dot %s'></span>", healthDotClass(data.QdrantStatus)))
	sb.WriteString(fmt.Sprintf("<span>Qdrant</span>"))
	if data.QdrantPoints > 0 {
		sb.WriteString(fmt.Sprintf("<span style='margin-left:auto;font-family:var(--mono)'>%d pts</span>", data.QdrantPoints))
	}
	sb.WriteString("</div>")
	sb.WriteString("<div class='health-row'>")
	sb.WriteString(fmt.Sprintf("<span class='health-dot %s'></span>", healthDotClass(data.EmbedderStatus)))
	sb.WriteString("<span>Embedder</span>")
	sb.WriteString(fmt.Sprintf("<span style='margin-left:auto;font-family:var(--mono)'>%s</span>", template.HTMLEscapeString(data.EmbedderModel)))
	sb.WriteString("</div>")
	// Model details
	sb.WriteString("<div class='health-model'>")
	sb.WriteString(fmt.Sprintf("<span>%s</span> &middot; <span>%d dim</span> &middot; <span>%s</span>",
		template.HTMLEscapeString(data.EmbedderBackend), data.EmbedderDim, formatUptime(data.Uptime)))
	sb.WriteString("</div>")
	sb.WriteString("</div>")
	return sb.String()
}

func healthDotClass(status string) string {
	switch status {
	case "green", "healthy":
		return "dot-green"
	case "yellow":
		return "dot-yellow"
	default:
		return "dot-red"
	}
}

func formatUptime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
