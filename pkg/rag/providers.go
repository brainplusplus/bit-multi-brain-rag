package rag

// ProviderSpec is the static metadata for an embedding provider:
// default endpoints, schema flavor, model discovery capabilities, and a
// curated list of recommended models. Used by the dashboard UI to populate
// the provider dropdown and model picker.
type ProviderSpec struct {
	ID                     string          `json:"id"`             // matches store.EmbedBackend
	DisplayName            string          `json:"display_name"`   // human label
	DefaultBaseURL         string          `json:"default_base_url"`
	Schema                 string          `json:"schema"`         // openai_v1 | cohere_v2 | ollama_native
	SupportsModelDiscovery bool            `json:"supports_model_discovery"`
	DiscoveryEndpoint      string          `json:"discovery_endpoint,omitempty"` // e.g. "/v1/models"
	RequiresAPIKey         bool            `json:"requires_api_key"`
	DockerHostNote         string          `json:"docker_host_note,omitempty"` // shown as warning for local providers
	CuratedModels          []CuratedModel  `json:"curated_models"`
}

// CuratedModel describes a known model for a provider with verified specs.
type CuratedModel struct {
	Name             string `json:"name"`               // wire model name (sent to API)
	Label            string `json:"label,omitempty"`    // optional friendly display name
	Dim              int    `json:"dim"`                // default embedding dimension
	MaxContextTokens int    `json:"max_context_tokens"` // model's hard context limit
	Recommended      bool   `json:"recommended"`        // highlighted in UI
	Notes            string `json:"notes,omitempty"`    // short description
}

// Providers returns the full provider registry. Order determines UI dropdown order.
func Providers() []ProviderSpec {
	return []ProviderSpec{
		{
			ID:                     "llama_q8",
			DisplayName:            "Local (llama-server)",
			DefaultBaseURL:         "http://embedder:8080/v1",
			Schema:                 "openai_v1",
			SupportsModelDiscovery: false,
			RequiresAPIKey:         false,
			DockerHostNote:         "Bundled embedder running in docker-compose. Default voyage-4-nano Q8 GGUF.",
			CuratedModels: []CuratedModel{
				{Name: "voyage-4-nano", Dim: 1024, MaxContextTokens: 32000, Recommended: true, Notes: "Default bundled GGUF (Voyage open-weight, Q8 quantized)"},
			},
		},
		{
			ID:                     "ollama",
			DisplayName:            "Ollama (local)",
			DefaultBaseURL:         "http://host.docker.internal:11434/v1",
			Schema:                 "openai_v1",
			SupportsModelDiscovery: true,
			DiscoveryEndpoint:      "/api/tags", // Ollama native; we'll adapt
			RequiresAPIKey:         false,
			DockerHostNote:         "If BitBrain runs in Docker, use http://host.docker.internal:11434/v1 instead of localhost. Make sure 'ollama serve' is running and you've pulled an embedding model (e.g. 'ollama pull nomic-embed-text').",
			CuratedModels: []CuratedModel{
				{Name: "nomic-embed-text", Dim: 768, MaxContextTokens: 8192, Recommended: true, Notes: "137M params, fast general-purpose"},
				{Name: "mxbai-embed-large", Dim: 1024, MaxContextTokens: 512, Notes: "334M params, higher quality but short context"},
				{Name: "bge-m3", Dim: 1024, MaxContextTokens: 8192, Notes: "Multilingual, 100+ languages"},
				{Name: "qwen3-embedding:0.6b", Dim: 1024, MaxContextTokens: 32768, Notes: "Best MTEB score among Ollama models (2026)"},
				{Name: "all-minilm", Dim: 384, MaxContextTokens: 256, Notes: "23M params, fastest but lowest quality"},
			},
		},
		{
			ID:                     "voyage",
			DisplayName:            "Voyage AI",
			DefaultBaseURL:         "https://api.voyageai.com/v1",
			Schema:                 "openai_v1",
			SupportsModelDiscovery: false, // Voyage doesn't expose /v1/models publicly
			RequiresAPIKey:         true,
			CuratedModels: []CuratedModel{
				{Name: "voyage-4-large", Dim: 1024, MaxContextTokens: 32000, Recommended: true, Notes: "Best general-purpose retrieval quality"},
				{Name: "voyage-4", Dim: 1024, MaxContextTokens: 32000, Notes: "Balanced quality and cost"},
				{Name: "voyage-4-lite", Dim: 1024, MaxContextTokens: 32000, Notes: "Optimized for latency and cost"},
				{Name: "voyage-4-nano", Dim: 1024, MaxContextTokens: 32000, Notes: "Smallest in v4 family, open-weight"},
				{Name: "voyage-code-3", Dim: 1024, MaxContextTokens: 32000, Recommended: true, Notes: "Optimized for code retrieval"},
				{Name: "voyage-3.5", Dim: 1024, MaxContextTokens: 32000, Notes: "Previous generation general-purpose"},
				{Name: "voyage-3.5-lite", Dim: 1024, MaxContextTokens: 32000, Notes: "Previous gen, cheapest"},
			},
		},
		{
			ID:                     "openai",
			DisplayName:            "OpenAI",
			DefaultBaseURL:         "https://api.openai.com/v1",
			Schema:                 "openai_v1",
			SupportsModelDiscovery: true,
			DiscoveryEndpoint:      "/v1/models",
			RequiresAPIKey:         true,
			CuratedModels: []CuratedModel{
				{Name: "text-embedding-3-small", Dim: 1536, MaxContextTokens: 8191, Recommended: true, Notes: "Best price/quality balance"},
				{Name: "text-embedding-3-large", Dim: 3072, MaxContextTokens: 8191, Notes: "Highest quality, larger vectors"},
				{Name: "text-embedding-ada-002", Dim: 1536, MaxContextTokens: 8191, Notes: "Legacy, prefer v3 models"},
			},
		},
		{
			ID:                     "cohere",
			DisplayName:            "Cohere",
			DefaultBaseURL:         "https://api.cohere.com",
			Schema:                 "cohere_v2",
			SupportsModelDiscovery: true,
			DiscoveryEndpoint:      "/v1/models?endpoint=embed",
			RequiresAPIKey:         true,
			CuratedModels: []CuratedModel{
				{Name: "embed-english-v3.0", Dim: 1024, MaxContextTokens: 512, Recommended: true, Notes: "English-only, high quality"},
				{Name: "embed-multilingual-v3.0", Dim: 1024, MaxContextTokens: 512, Notes: "100+ languages"},
				{Name: "embed-english-light-v3.0", Dim: 384, MaxContextTokens: 512, Notes: "Faster, smaller vectors"},
			},
		},
		{
			ID:                     "openrouter",
			DisplayName:            "OpenRouter",
			DefaultBaseURL:         "https://openrouter.ai/api/v1",
			Schema:                 "openai_v1",
			SupportsModelDiscovery: true,
			DiscoveryEndpoint:      "/api/v1/models",
			RequiresAPIKey:         true,
			CuratedModels: []CuratedModel{
				{Name: "openai/text-embedding-3-small", Dim: 1536, MaxContextTokens: 8191, Recommended: true, Notes: "OpenAI via OpenRouter proxy"},
				{Name: "openai/text-embedding-3-large", Dim: 3072, MaxContextTokens: 8191, Notes: "OpenAI large via OpenRouter"},
				{Name: "voyage/voyage-3-large", Dim: 1024, MaxContextTokens: 32000, Notes: "Voyage via OpenRouter"},
			},
		},
	}
}

// GetProvider returns the spec for a given provider ID, or false if unknown.
func GetProvider(id string) (ProviderSpec, bool) {
	for _, p := range Providers() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderSpec{}, false
}

// LookupCuratedModel returns the curated model spec for a (provider, modelName) pair.
// Used by the indexer to derive smart chunk size defaults from MaxContextTokens.
func LookupCuratedModel(providerID, modelName string) (CuratedModel, bool) {
	p, ok := GetProvider(providerID)
	if !ok {
		return CuratedModel{}, false
	}
	for _, m := range p.CuratedModels {
		if m.Name == modelName {
			return m, true
		}
	}
	return CuratedModel{}, false
}
