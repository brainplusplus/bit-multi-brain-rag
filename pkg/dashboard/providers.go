package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
)

// apiProviders returns the full provider registry (static metadata + curated models).
func (s *Server) apiProviders(c echo.Context) error {
	return c.JSON(200, map[string]any{"providers": rag.Providers()})
}

// apiProviderModels returns the model list for a provider. If ?refresh=1 and
// the provider supports model discovery, attempts to live-fetch and merge
// with the curated list (deduped by name). Falls back gracefully on errors.
func (s *Server) apiProviderModels(c echo.Context) error {
	id := c.Param("id")
	p, ok := rag.GetProvider(id)
	if !ok {
		return c.JSON(404, map[string]string{"error": "unknown provider: " + id})
	}
	curated := append([]rag.CuratedModel{}, p.CuratedModels...)
	live := []rag.CuratedModel{}
	var refreshErr string
	if c.QueryParam("refresh") == "1" && p.SupportsModelDiscovery {
		apiKey := c.QueryParam("api_key")
		baseURL := c.QueryParam("base_url")
		if baseURL == "" {
			baseURL = p.DefaultBaseURL
		}
		fetched, err := fetchProviderModels(c.Request().Context(), p, baseURL, apiKey)
		if err != nil {
			refreshErr = err.Error()
		} else {
			live = fetched
		}
	}
	// Merge: curated first (recommended order), then live not in curated.
	seen := map[string]bool{}
	merged := make([]rag.CuratedModel, 0, len(curated)+len(live))
	for _, m := range curated {
		merged = append(merged, m)
		seen[m.Name] = true
	}
	for _, m := range live {
		if !seen[m.Name] {
			merged = append(merged, m)
		}
	}
	return c.JSON(200, map[string]any{
		"provider":      p.ID,
		"models":        merged,
		"refresh_error": refreshErr,
	})
}

// fetchProviderModels does a best-effort live model discovery call. Returns
// only embedding-suitable models (filtered by name heuristics per provider).
func fetchProviderModels(ctx context.Context, p rag.ProviderSpec, baseURL, apiKey string) ([]rag.CuratedModel, error) {
	switch p.Schema {
	case "openai_v1":
		// GET {baseURL}/models -> {data:[{id, ...}, ...]}
		url := strings.TrimRight(baseURL, "/") + "/models"
		// Ollama exposes /api/tags, not /v1/models; check for Ollama by host
		if p.ID == "ollama" {
			url = ollamaTagsURL(baseURL)
		}
		body, err := httpGetJSON(ctx, url, apiKey)
		if err != nil {
			return nil, err
		}
		if p.ID == "ollama" {
			return parseOllamaTags(body), nil
		}
		return parseOpenAIModels(body, p.ID), nil
	case "cohere_v2":
		url := strings.TrimRight(baseURL, "/") + "/v1/models?endpoint=embed"
		body, err := httpGetJSON(ctx, url, apiKey)
		if err != nil {
			return nil, err
		}
		return parseCohereModels(body), nil
	}
	return nil, fmt.Errorf("model discovery not implemented for schema %s", p.Schema)
}

func ollamaTagsURL(baseURL string) string {
	// User likely gave http://host:11434/v1, but /api/tags lives at root
	trimmed := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(trimmed, "/v1") {
		trimmed = strings.TrimSuffix(trimmed, "/v1")
	}
	return trimmed + "/api/tags"
}

func httpGetJSON(ctx context.Context, url, apiKey string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
		if len(buf) > 1<<20 {
			break // 1 MB cap
		}
	}
	return buf, nil
}

func parseOpenAIModels(body []byte, providerID string) []rag.CuratedModel {
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	var out []rag.CuratedModel
	for _, m := range r.Data {
		name := m.ID
		lower := strings.ToLower(name)
		// Heuristic: keep only models that look like embedders.
		if !(strings.Contains(lower, "embed") || strings.Contains(lower, "voyage") || strings.Contains(lower, "bge")) {
			continue
		}
		out = append(out, rag.CuratedModel{Name: name, Notes: "Discovered via /v1/models"})
	}
	return out
}

func parseOllamaTags(body []byte) []rag.CuratedModel {
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	var out []rag.CuratedModel
	for _, m := range r.Models {
		out = append(out, rag.CuratedModel{Name: m.Name, Notes: "Installed locally"})
	}
	return out
}

func parseCohereModels(body []byte) []rag.CuratedModel {
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	var out []rag.CuratedModel
	for _, m := range r.Models {
		out = append(out, rag.CuratedModel{Name: m.Name})
	}
	return out
}

// apiUpdateModel patches an existing model (PATCH /api/v1/models/:id).
func (s *Server) apiUpdateModel(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(400, map[string]string{"error": "invalid id"})
	}
	existing, err := s.store.GetModel(c.Request().Context(), id)
	if err != nil {
		return c.JSON(404, map[string]string{"error": err.Error()})
	}
	var req store.EmbeddingModel
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid body"})
	}
	// Patch only mutable fields.
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.ModelName != "" {
		existing.ModelName = req.ModelName
	}
	if req.Endpoint != "" {
		existing.Endpoint = req.Endpoint
	}
	if req.APIKey != "" && !isMasked(req.APIKey) {
		existing.APIKey = req.APIKey
	}
	if req.Dim > 0 {
		existing.Dim = req.Dim
	}
	if req.Pooling != "" {
		existing.Pooling = req.Pooling
	}
	if req.MaxContextTokens > 0 {
		existing.MaxContextTokens = req.MaxContextTokens
	}
	if req.ChunkTokens != nil {
		existing.ChunkTokens = req.ChunkTokens
	}
	if req.ChunkOverlap > 0 {
		existing.ChunkOverlap = req.ChunkOverlap
	}
	if err := s.store.UpdateModel(c.Request().Context(), existing); err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	// If this is the active model, hot-swap with new chunk size.
	if existing.IsActive {
		_ = s.hotSwapEmbedder(existing)
	}
	existing.APIKey = maskAPIKey(existing.APIKey)
	return c.JSON(200, existing)
}

func isMasked(k string) bool {
	return strings.Contains(k, "...")
}

// --- UI handlers ---

// uiNewModelForm renders the multi-step add-model form replacing the old flat form.
func (s *Server) uiNewModelForm(c echo.Context) error {
	return c.HTML(200, s.renderModelForm(nil))
}

func (s *Server) uiEditModelForm(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	m, err := s.store.GetModel(c.Request().Context(), id)
	if err != nil {
		return c.HTML(404, "<p class='error'>Model not found</p>")
	}
	return c.HTML(200, s.renderModelForm(&m))
}

// uiProviderModelOptions returns <option> elements for the model picker
// when the user changes the provider dropdown (HTMX swap).
func (s *Server) uiProviderModelOptions(c echo.Context) error {
	id := c.Param("id")
	p, ok := rag.GetProvider(id)
	if !ok {
		return c.HTML(404, "<option value=''>Unknown provider</option>")
	}
	var sb strings.Builder
	sb.WriteString("<option value=''>Select a model...</option>")
	for _, m := range p.CuratedModels {
		sel := ""
		marker := ""
		if m.Recommended {
			marker = " ★"
		}
		// Encode dim + ctx into option dataset attrs for client-side autofill via HTMX swap of advanced fields.
		sb.WriteString(fmt.Sprintf(
			"<option value='%s' data-dim='%d' data-ctx='%d'%s>%s%s — %dd · %dk ctx</option>",
			template.HTMLEscapeString(m.Name), m.Dim, m.MaxContextTokens, sel,
			template.HTMLEscapeString(m.Name), marker, m.Dim, m.MaxContextTokens/1000,
		))
	}
	return c.HTML(200, sb.String())
}

func (s *Server) uiUpdateModel(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	existing, err := s.store.GetModel(c.Request().Context(), id)
	if err != nil {
		return c.HTML(404, "<p class='error'>Model not found</p>")
	}
	existing.Name = c.FormValue("name")
	existing.ModelName = c.FormValue("model_name")
	existing.Endpoint = c.FormValue("endpoint")
	if k := c.FormValue("api_key"); k != "" && !isMasked(k) {
		existing.APIKey = k
	}
	existing.Dim = atoiDefault(c.FormValue("dim"), existing.Dim)
	existing.Pooling = c.FormValue("pooling")
	existing.MaxContextTokens = atoiDefault(c.FormValue("max_context_tokens"), existing.MaxContextTokens)
	if cs := strings.TrimSpace(c.FormValue("chunk_tokens")); cs != "" {
		if v, err := strconv.Atoi(cs); err == nil && v > 0 {
			existing.ChunkTokens = &v
		}
	} else {
		existing.ChunkTokens = nil // back to auto
	}
	if err := s.store.UpdateModel(c.Request().Context(), existing); err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	if existing.IsActive {
		_ = s.hotSwapEmbedder(existing)
	}
	models, _ := s.store.ListModels(c.Request().Context())
	return c.HTML(200, s.renderModelsPanelBody(models))
}
