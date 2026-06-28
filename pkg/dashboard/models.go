package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
)

// --- API handlers ---

func (s *Server) apiListModels(c echo.Context) error {
	models, err := s.store.ListModels(c.Request().Context())
	if err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	// Mask API keys in response.
	for i := range models {
		models[i].APIKey = maskAPIKey(models[i].APIKey)
	}
	return c.JSON(200, map[string]any{"models": models})
}

func (s *Server) apiCreateModel(c echo.Context) error {
	var req store.EmbeddingModel
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Name == "" || req.ModelName == "" || req.Backend == "" {
		return c.JSON(400, map[string]string{"error": "name, model_name, and backend are required"})
	}
	if req.Dim <= 0 {
		return c.JSON(400, map[string]string{"error": "dim must be positive"})
	}
	created, err := s.store.CreateModel(c.Request().Context(), req)
	if err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	created.APIKey = maskAPIKey(created.APIKey)
	return c.JSON(201, created)
}

func (s *Server) apiDeleteModel(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(400, map[string]string{"error": "invalid model id"})
	}
	if err := s.store.DeleteModel(c.Request().Context(), id); err != nil {
		return c.JSON(400, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, map[string]string{"status": "deleted"})
}

func (s *Server) apiSetActiveModel(c echo.Context) error {
	var req struct {
		ModelID int64 `json:"model_id"`
	}
	if err := c.Bind(&req); err != nil || req.ModelID <= 0 {
		return c.JSON(400, map[string]string{"error": "model_id is required"})
	}
	if err := s.store.SetActiveModel(c.Request().Context(), req.ModelID); err != nil {
		return c.JSON(400, map[string]string{"error": err.Error()})
	}
	// Hot-swap the embedder.
	model, err := s.store.GetModel(c.Request().Context(), req.ModelID)
	if err != nil {
		return c.JSON(500, map[string]string{"error": "model set but failed to load: " + err.Error()})
	}
	if err := s.hotSwapEmbedder(model); err != nil {
		return c.JSON(500, map[string]string{"error": "model set but adapter creation failed: " + err.Error()})
	}
	return c.JSON(200, map[string]any{
		"status":  "active model changed",
		"model":   model.Name,
		"warning": "Existing indexes remain valid. Re-index projects to use the new model.",
	})
}

func (s *Server) apiCompare(c echo.Context) error {
	var req struct {
		Project  string  `json:"project"`
		Query    string  `json:"query"`
		ModelIDs []int64 `json:"model_ids"`
		Limit    int     `json:"limit"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Project == "" || req.Query == "" || len(req.ModelIDs) < 2 {
		return c.JSON(400, map[string]string{"error": "project, query, and at least 2 model_ids required"})
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	results, err := s.runComparison(c.Request().Context(), req.Project, req.Query, req.ModelIDs, req.Limit)
	if err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, results)
}

// --- UI handlers ---

func (s *Server) uiModelsPanel(c echo.Context) error {
	models, err := s.store.ListModels(c.Request().Context())
	if err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	return c.HTML(200, s.renderModelsPanel(models))
}

func (s *Server) uiCreateModel(c echo.Context) error {
	provider := c.FormValue("provider")
	m := store.EmbeddingModel{
		Name:             c.FormValue("name"),
		Backend:          store.EmbedBackend(provider),
		ModelName:        c.FormValue("model_name"),
		Endpoint:         c.FormValue("endpoint"),
		APIKey:           c.FormValue("api_key"),
		Dim:              atoiDefault(c.FormValue("dim"), 1024),
		Pooling:          c.FormValue("pooling"),
		MaxContextTokens: atoiDefault(c.FormValue("max_context_tokens"), 0),
	}
	// Auto-fill from curated registry if user didn't override.
	if curated, ok := rag.LookupCuratedModel(provider, m.ModelName); ok {
		if m.Dim == 0 || m.Dim == 1024 && curated.Dim > 0 {
			m.Dim = curated.Dim
		}
		if m.MaxContextTokens == 0 {
			m.MaxContextTokens = curated.MaxContextTokens
		}
	}
	if cs := strings.TrimSpace(c.FormValue("chunk_tokens")); cs != "" {
		if v, err := strconv.Atoi(cs); err == nil && v > 0 {
			m.ChunkTokens = &v
		}
	}
	if m.Name == "" || m.ModelName == "" || m.Backend == "" {
		return c.HTML(400, "<p class='error'>Name, provider, and model are required.</p>")
	}
	// Default endpoint from provider spec if user left empty.
	if m.Endpoint == "" {
		if p, ok := rag.GetProvider(string(m.Backend)); ok {
			m.Endpoint = p.DefaultBaseURL
		}
	}
	if _, err := s.store.CreateModel(c.Request().Context(), m); err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	models, _ := s.store.ListModels(c.Request().Context())
	return c.HTML(200, s.renderModelsPanelBody(models))
}

func (s *Server) uiDeleteModel(c echo.Context) error {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := s.store.DeleteModel(c.Request().Context(), id); err != nil {
		return c.HTML(400, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	models, _ := s.store.ListModels(c.Request().Context())
	return c.HTML(200, s.renderModelsPanelBody(models))
}

func (s *Server) uiSetActiveModel(c echo.Context) error {
	id, _ := strconv.ParseInt(c.FormValue("model_id"), 10, 64)
	if id <= 0 {
		return c.HTML(400, "<p class='error'>Invalid model ID</p>")
	}
	if err := s.store.SetActiveModel(c.Request().Context(), id); err != nil {
		return c.HTML(400, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	model, err := s.store.GetModel(c.Request().Context(), id)
	if err == nil {
		_ = s.hotSwapEmbedder(model)
	}
	models, _ := s.store.ListModels(c.Request().Context())
	return c.HTML(200, s.renderModelsPanelBody(models))
}

func (s *Server) uiCompare(c echo.Context) error {
	project := c.FormValue("project")
	query := c.FormValue("query")
	modelIDsStr := c.FormValue("model_ids")
	if project == "" || query == "" || modelIDsStr == "" {
		return c.HTML(400, "<p class='error'>Project, query, and model_ids required.</p>")
	}
	var modelIDs []int64
	for _, s := range strings.Split(modelIDsStr, ",") {
		id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if id > 0 {
			modelIDs = append(modelIDs, id)
		}
	}
	if len(modelIDs) < 2 {
		return c.HTML(400, "<p class='error'>Select at least 2 models to compare.</p>")
	}
	results, err := s.runComparison(c.Request().Context(), project, query, modelIDs, 5)
	if err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	return c.HTML(200, s.renderCompareResults(query, results))
}

// --- core logic ---

// embedMu protects hot-swap of s.embed and s.indexer.
var embedMu sync.RWMutex

func (s *Server) hotSwapEmbedder(model store.EmbeddingModel) error {
	client, err := rag.NewEmbedderFromConfig(rag.EmbedModelConfig{
		Backend:   string(model.Backend),
		ModelName: model.ModelName,
		Endpoint:  model.Endpoint,
		APIKey:    model.APIKey,
		Dim:       model.Dim,
		Pooling:   model.Pooling,
		Timeout:   60 * time.Second,
	})
	if err != nil {
		return err
	}
	embedMu.Lock()
	s.embed = client
	// Rebuild indexer with new embedder + per-model chunk size.
	idx := indexer.New(chunker.New(), client, s.rag, s.logger)
	idx.MaxTokensPerChunk = model.EffectiveChunkTokens()
	idx.HybridEnabled = s.indexer.HybridEnabled // preserve hybrid setting across hot-swap
	s.indexer = idx
	embedMu.Unlock()
	s.logger.Info("hot-swapped embedder",
		"model", model.ModelName,
		"backend", string(model.Backend),
		"dim", model.Dim,
		"chunk_tokens", idx.MaxTokensPerChunk,
		"max_ctx", model.MaxContextTokens,
	)
	return nil
}

type compareColumn struct {
	Model   store.EmbeddingModel `json:"model"`
	Results []rag.Result         `json:"results"`
	Latency time.Duration        `json:"latency_ms"`
	Error   string               `json:"error,omitempty"`
}

func (s *Server) runComparison(ctx context.Context, project, query string, modelIDs []int64, limit int) ([]compareColumn, error) {
	columns := make([]compareColumn, len(modelIDs))
	var wg sync.WaitGroup
	for i, id := range modelIDs {
		wg.Add(1)
		go func(idx int, modelID int64) {
			defer wg.Done()
			model, err := s.store.GetModel(ctx, modelID)
			if err != nil {
				columns[idx] = compareColumn{Error: err.Error()}
				return
			}
			columns[idx].Model = model
			// Create a temporary embedder for this model.
			client, err := rag.NewEmbedderFromConfig(rag.EmbedModelConfig{
				Backend:   string(model.Backend),
				ModelName: model.ModelName,
				Endpoint:  model.Endpoint,
				APIKey:    model.APIKey,
				Dim:       model.Dim,
				Pooling:   model.Pooling,
				Timeout:   30 * time.Second,
			})
			if err != nil {
				columns[idx].Error = "adapter: " + err.Error()
				return
			}
			start := time.Now()
			// Embed query.
			vecs, err := client.Embed(ctx, []string{query})
			if err != nil {
				columns[idx].Error = "embed: " + err.Error()
				columns[idx].Latency = time.Since(start)
				return
			}
			if len(vecs) == 0 {
				columns[idx].Error = "embed returned 0 vectors"
				return
			}
			// Build collection key for this model.
			key := rag.CollectionKey{
				Project: project,
				Domain:  rag.DomainCode,
				Model:   model.ModelName,
				Dim:     model.Dim,
				Backend: string(model.Backend),
			}
			results, err := s.rag.SemanticSearch(ctx, key, vecs[0], limit)
			columns[idx].Latency = time.Since(start)
			if err != nil {
				columns[idx].Error = "search: " + err.Error()
				return
			}
			columns[idx].Results = results
		}(i, id)
	}
	wg.Wait()
	return columns, nil
}

// --- rendering ---

// renderModelsPanel is the top-level page renderer for /ui/models.
func (s *Server) renderModelsPanel(models []store.EmbeddingModel) string {
	var sb strings.Builder
	sb.WriteString("<div id='main' class='main models-panel'>")
	sb.WriteString("<div class='models-header'>")
	sb.WriteString("<div><h2>Embedding Models</h2>")
	sb.WriteString("<p class='muted small'>Switch the active model at runtime. Existing indexes remain valid under their original model.</p></div>")
	sb.WriteString("<button class='btn' hx-get='/ui/models/new' hx-target='#models-panel-body' hx-swap='innerHTML'>+ Add model</button>")
	sb.WriteString("</div>")
	sb.WriteString("<div id='models-panel-body'>")
	sb.WriteString(s.renderModelsPanelBody(models))
	sb.WriteString("</div>")
	sb.WriteString("</div>")
	return sb.String()
}

// renderModelsPanelBody is the swappable inner section (used after edit/create/delete).
func (s *Server) renderModelsPanelBody(models []store.EmbeddingModel) string {
	return s.renderModelsList(models)
}

func (s *Server) renderModelsList(models []store.EmbeddingModel) string {
	if len(models) == 0 {
		return "<p class='muted small' style='margin-top:24px'>No models configured. Click <strong>+ Add model</strong> above.</p>"
	}
	var sb strings.Builder
	sb.WriteString("<div class='models-grid'>")
	for _, m := range models {
		sb.WriteString("<div class='model-card")
		if m.IsActive {
			sb.WriteString(" model-active")
		}
		sb.WriteString("'>")
		sb.WriteString("<div class='model-card-head'>")
		sb.WriteString(fmt.Sprintf("<h3>%s</h3>", template.HTMLEscapeString(m.Name)))
		if m.IsActive {
			sb.WriteString("<span class='badge accent'>active</span>")
		}
		sb.WriteString("</div>")
		// Provider/model line
		providerLabel := string(m.Backend)
		if p, ok := rag.GetProvider(string(m.Backend)); ok {
			providerLabel = p.DisplayName
		}
		sb.WriteString("<div class='model-meta'>")
		sb.WriteString(fmt.Sprintf("<span class='meta-chip'>%s</span>", template.HTMLEscapeString(providerLabel)))
		sb.WriteString(fmt.Sprintf("<span class='meta-chip mono'>%s</span>", template.HTMLEscapeString(m.ModelName)))
		sb.WriteString(fmt.Sprintf("<span class='meta-chip'>%d dim</span>", m.Dim))
		if m.MaxContextTokens > 0 {
			sb.WriteString(fmt.Sprintf("<span class='meta-chip'>%dk ctx</span>", m.MaxContextTokens/1000))
		}
		// Effective chunk size
		eff := m.EffectiveChunkTokens()
		chunkLabel := fmt.Sprintf("%d tok chunks", eff)
		if m.ChunkTokens == nil {
			chunkLabel = fmt.Sprintf("auto: %d tok chunks", eff)
		}
		sb.WriteString(fmt.Sprintf("<span class='meta-chip'>%s</span>", chunkLabel))
		sb.WriteString("</div>")
		if m.Endpoint != "" {
			sb.WriteString(fmt.Sprintf("<div class='model-endpoint mono'>%s</div>", template.HTMLEscapeString(m.Endpoint)))
		}
		sb.WriteString("<div class='model-actions'>")
		if !m.IsActive {
			sb.WriteString(fmt.Sprintf("<form hx-post='/ui/models/active' hx-target='#models-panel-body' hx-swap='innerHTML' style='display:inline'>"+
				"<input type='hidden' name='model_id' value='%d'>"+
				"<button type='submit' class='btn'>Activate</button></form>", m.ID))
		}
		sb.WriteString(fmt.Sprintf("<button class='ghost-btn' hx-get='/ui/models/%d/edit' hx-target='#models-panel-body' hx-swap='innerHTML'>Edit</button>", m.ID))
		if !m.IsActive {
			sb.WriteString(fmt.Sprintf("<form hx-post='/ui/models/%d/delete' hx-target='#models-panel-body' hx-swap='innerHTML' hx-confirm='Delete %s?' style='display:inline'>"+
				"<button type='submit' class='ghost-btn danger'>Delete</button></form>", m.ID, template.HTMLEscapeString(m.Name)))
		}
		sb.WriteString("</div></div>")
	}
	sb.WriteString("</div>")
	return sb.String()
}

// renderModelForm renders the add/edit model wizard. If model is nil, this is
// a new-model form; otherwise it's an edit form (with prefilled values + PATCH target).
func (s *Server) renderModelForm(model *store.EmbeddingModel) string {
	providers := rag.Providers()
	isEdit := model != nil
	selectedProvider := ""
	selectedModelName := ""
	selectedDim := 0
	selectedMaxCtx := 0
	selectedEndpoint := ""
	selectedAPIKey := ""
	selectedPooling := "mean"
	chunkOverride := ""
	selectedName := ""
	if isEdit {
		selectedProvider = string(model.Backend)
		selectedModelName = model.ModelName
		selectedDim = model.Dim
		selectedMaxCtx = model.MaxContextTokens
		selectedEndpoint = model.Endpoint
		selectedAPIKey = maskAPIKey(model.APIKey)
		selectedPooling = model.Pooling
		if model.ChunkTokens != nil {
			chunkOverride = strconv.Itoa(*model.ChunkTokens)
		}
		selectedName = model.Name
	}
	// Build provider->endpoint+note JSON for client-side autofill.
	providerMeta, _ := json.Marshal(providersToMetaMap(providers))

	var sb strings.Builder
	target := "/ui/models"
	method := "hx-post"
	heading := "Add embedding model"
	submitLabel := "Save and activate"
	if isEdit {
		target = fmt.Sprintf("/ui/models/%d", model.ID)
		heading = "Edit model"
		submitLabel = "Save changes"
	}

	sb.WriteString("<div class='model-form-wrapper'>")
	sb.WriteString(fmt.Sprintf("<div class='model-form-head'><h3>%s</h3>", heading))
	sb.WriteString("<button class='ghost-btn' hx-get='/ui/models' hx-target='#main' hx-swap='outerHTML'>Cancel</button></div>")

	sb.WriteString(fmt.Sprintf("<form id='model-form' %s='%s' hx-target='#main' hx-swap='outerHTML' class='model-form'>", method, target))

	// === Provider section ===
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Provider</label>")
	if isEdit {
		// Provider is immutable after creation (backend defines schema).
		sb.WriteString("<input type='hidden' name='provider' value='" + template.HTMLEscapeString(selectedProvider) + "'>")
		pLabel := selectedProvider
		if p, ok := rag.GetProvider(selectedProvider); ok {
			pLabel = p.DisplayName
		}
		sb.WriteString(fmt.Sprintf("<div class='form-readonly'>%s <span class='muted small'>(cannot change after creation)</span></div>", template.HTMLEscapeString(pLabel)))
	} else {
		sb.WriteString("<select name='provider' required class='form-input' ")
		sb.WriteString("hx-get='/ui/providers/__id__/models' hx-trigger='change' hx-target='#model-select' hx-swap='innerHTML' ")
		sb.WriteString("onchange=\"providerChanged(this)\">")
		sb.WriteString("<option value=''>Choose a provider...</option>")
		for _, p := range providers {
			sel := ""
			if p.ID == selectedProvider {
				sel = " selected"
			}
			sb.WriteString(fmt.Sprintf("<option value='%s'%s>%s</option>", template.HTMLEscapeString(p.ID), sel, template.HTMLEscapeString(p.DisplayName)))
		}
		sb.WriteString("</select>")
	}
	sb.WriteString("<div id='provider-note' class='form-note'></div>")
	sb.WriteString("</div>")

	// === Model section ===
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Model</label>")
	sb.WriteString("<div class='form-row'>")
	sb.WriteString("<select name='model_name' id='model-select' required class='form-input' onchange='modelChanged(this)'>")
	if isEdit {
		// Pre-populate options for the current provider.
		if p, ok := rag.GetProvider(selectedProvider); ok {
			for _, m := range p.CuratedModels {
				sel := ""
				if m.Name == selectedModelName {
					sel = " selected"
				}
				sb.WriteString(fmt.Sprintf(
					"<option value='%s' data-dim='%d' data-ctx='%d'%s>%s — %dd · %dk ctx</option>",
					template.HTMLEscapeString(m.Name), m.Dim, m.MaxContextTokens, sel,
					template.HTMLEscapeString(m.Name), m.Dim, m.MaxContextTokens/1000,
				))
			}
		}
	} else {
		sb.WriteString("<option value=''>Select a provider first...</option>")
	}
	sb.WriteString("</select>")
	sb.WriteString("<button type='button' class='ghost-btn' onclick='refreshModels()' title='Refresh from provider /v1/models'>↻</button>")
	sb.WriteString("</div>")
	sb.WriteString("</div>")

	// === Display name ===
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Display name</label>")
	sb.WriteString(fmt.Sprintf("<input name='name' class='form-input' value='%s' placeholder='e.g. Voyage Code 3 (production)' required>",
		template.HTMLEscapeString(selectedName)))
	sb.WriteString("</div>")

	// === Advanced (collapsed) ===
	sb.WriteString("<details class='form-advanced'><summary>Advanced</summary>")
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Base URL <span class='muted small'>(empty = provider default)</span></label>")
	sb.WriteString(fmt.Sprintf("<input name='endpoint' id='endpoint-input' class='form-input mono' value='%s' placeholder='https://api.example.com/v1'>",
		template.HTMLEscapeString(selectedEndpoint)))
	sb.WriteString("</div>")
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>API key</label>")
	sb.WriteString(fmt.Sprintf("<input name='api_key' type='password' class='form-input' value='%s' placeholder='sk-...'>",
		template.HTMLEscapeString(selectedAPIKey)))
	sb.WriteString("</div>")
	sb.WriteString("<div class='form-row form-grid-2'>")
	sb.WriteString("<div><label class='form-label'>Embedding dim</label>")
	sb.WriteString(fmt.Sprintf("<input name='dim' id='dim-input' type='number' class='form-input' value='%d' min='1'></div>", selectedDim))
	sb.WriteString("<div><label class='form-label'>Max context tokens</label>")
	sb.WriteString(fmt.Sprintf("<input name='max_context_tokens' id='ctx-input' type='number' class='form-input' value='%d' min='0'></div>", selectedMaxCtx))
	sb.WriteString("</div>")
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Chunk size (tokens) <span class='muted small'>— leave blank for smart auto</span></label>")
	sb.WriteString(fmt.Sprintf("<input name='chunk_tokens' id='chunk-input' type='number' class='form-input' value='%s' placeholder='auto'>",
		template.HTMLEscapeString(chunkOverride)))
	sb.WriteString("<div class='form-note warn'>⚠ Changing chunk size requires re-indexing projects using this model.</div>")
	sb.WriteString("</div>")
	sb.WriteString("<div class='form-section'>")
	sb.WriteString("<label class='form-label'>Pooling (llama-server only)</label>")
	sb.WriteString(fmt.Sprintf("<input name='pooling' class='form-input' value='%s' placeholder='mean | cls'>", template.HTMLEscapeString(selectedPooling)))
	sb.WriteString("</div>")
	sb.WriteString("</details>")

	// === Submit ===
	sb.WriteString("<div class='form-actions'>")
	sb.WriteString(fmt.Sprintf("<button type='submit' class='btn'>%s</button>", submitLabel))
	sb.WriteString("<button type='button' class='ghost-btn' hx-get='/ui/models' hx-target='#main' hx-swap='outerHTML'>Cancel</button>")
	sb.WriteString("</div>")
	sb.WriteString("</form>")

	// === Provider metadata for client-side autofill ===
	sb.WriteString(fmt.Sprintf("<script>window.providerMeta=%s;%s</script>", string(providerMeta), modelFormJS))
	sb.WriteString("</div>")
	return sb.String()
}

type providerMeta struct {
	DefaultBaseURL string `json:"default_base_url"`
	Note           string `json:"note"`
	APIKeyRequired bool   `json:"requires_api_key"`
}

func providersToMetaMap(providers []rag.ProviderSpec) map[string]providerMeta {
	out := map[string]providerMeta{}
	for _, p := range providers {
		out[p.ID] = providerMeta{
			DefaultBaseURL: p.DefaultBaseURL,
			Note:           p.DockerHostNote,
			APIKeyRequired: p.RequiresAPIKey,
		}
	}
	return out
}

const modelFormJS = `
function providerChanged(sel){
  var pid = sel.value;
  // Rewrite hx-get url placeholder so HTMX fetches model list for this provider.
  sel.setAttribute('hx-get', '/ui/providers/'+encodeURIComponent(pid)+'/models');
  htmx.process(sel);
  // Update note + default base URL
  var meta = (window.providerMeta||{})[pid];
  var note = document.getElementById('provider-note');
  var endpoint = document.getElementById('endpoint-input');
  if(meta){
    note.textContent = meta.note || '';
    note.className = meta.note ? 'form-note warn' : 'form-note';
    if(endpoint && !endpoint.value){ endpoint.placeholder = meta.default_base_url; }
  } else {
    note.textContent='';
  }
  // Trigger fetch by simulating change again now that hx-get is set.
  htmx.trigger(sel, 'change');
}
function modelChanged(sel){
  var opt = sel.options[sel.selectedIndex];
  if(!opt) return;
  var dim = parseInt(opt.dataset.dim||'0',10);
  var ctx = parseInt(opt.dataset.ctx||'0',10);
  var dimI = document.getElementById('dim-input');
  var ctxI = document.getElementById('ctx-input');
  var chunkI = document.getElementById('chunk-input');
  if(dim>0 && dimI){ dimI.value = dim; }
  if(ctx>0 && ctxI){ ctxI.value = ctx; }
  // Update chunk-size placeholder to show computed auto value
  if(ctx>0 && chunkI){
    var auto = Math.min(Math.floor(ctx*0.8), 2000);
    chunkI.placeholder = 'auto ('+auto+')';
  }
  // Auto-fill display name if blank.
  var nameI = document.querySelector('input[name="name"]');
  if(nameI && !nameI.value){ nameI.value = opt.value; }
}
function refreshModels(){
  var prov = document.querySelector('select[name="provider"]');
  var apiKey = document.querySelector('input[name="api_key"]').value;
  var baseURL = document.querySelector('input[name="endpoint"]').value;
  if(!prov || !prov.value){ alert('Pick a provider first.'); return; }
  fetch('/api/v1/providers/'+encodeURIComponent(prov.value)+'/models?refresh=1&api_key='+encodeURIComponent(apiKey)+'&base_url='+encodeURIComponent(baseURL),{
    headers:{'X-API-Key':'dev-key'}
  }).then(function(r){return r.json()}).then(function(data){
    var sel = document.getElementById('model-select');
    sel.innerHTML = '<option value="">Select a model...</option>';
    (data.models||[]).forEach(function(m){
      var o = document.createElement('option');
      o.value = m.name;
      o.dataset.dim = m.dim||0;
      o.dataset.ctx = m.max_context_tokens||0;
      o.textContent = m.name + (m.dim?' — '+m.dim+'d':'') + (m.max_context_tokens?' · '+Math.floor(m.max_context_tokens/1000)+'k ctx':'');
      sel.appendChild(o);
    });
    if(data.refresh_error){
      alert('Refresh failed: '+data.refresh_error+'\n\nShowing curated list only.');
    }
  });
}
`

func (s *Server) renderCompareResults(query string, columns []compareColumn) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<h3>Comparison: &ldquo;%s&rdquo;</h3>", template.HTMLEscapeString(query)))
	sb.WriteString("<div class='compare-grid'>")
	for _, col := range columns {
		sb.WriteString("<div class='compare-col'>")
		sb.WriteString(fmt.Sprintf("<h4>%s</h4>", template.HTMLEscapeString(col.Model.Name)))
		sb.WriteString(fmt.Sprintf("<p class='muted small'>%s · %s · %dms</p>",
			template.HTMLEscapeString(string(col.Model.Backend)),
			template.HTMLEscapeString(col.Model.ModelName),
			col.Latency.Milliseconds()))
		if col.Error != "" {
			sb.WriteString(fmt.Sprintf("<p class='error small'>%s</p>", template.HTMLEscapeString(col.Error)))
		} else if len(col.Results) == 0 {
			sb.WriteString("<p class='muted small'>No results (not indexed with this model?)</p>")
		} else {
			for _, r := range col.Results {
				sb.WriteString(fmt.Sprintf("<div class='compare-result'><span class='%s'>%.3f</span> <span class='result-file'>%s</span></div>",
					scoreClass(r.Score), r.Score, template.HTMLEscapeString(orFallback(r.Meta["source_file"], "?"))))
			}
		}
		sb.WriteString("</div>")
	}
	sb.WriteString("</div>")
	return sb.String()
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
