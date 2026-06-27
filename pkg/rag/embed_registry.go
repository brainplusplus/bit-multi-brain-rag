package rag

import (
	"fmt"
	"time"
)

// EmbedModelConfig is the minimal config needed to instantiate any embedding adapter.
// Matches the store.EmbeddingModel fields relevant to client creation.
type EmbedModelConfig struct {
	Backend   string // "llama_q8", "openai", "cohere", "voyage", "openrouter"
	ModelName string // wire model name
	Endpoint  string // base URL (empty = provider default)
	APIKey    string
	Dim       int
	Pooling   string // "mean", "cls", "" (only relevant for llama_q8)
	Timeout   time.Duration
}

// NewEmbedderFromConfig is the factory function that creates the appropriate
// EmbeddingClient based on the backend type. This is the single point where
// new backends are registered.
func NewEmbedderFromConfig(cfg EmbedModelConfig) (EmbeddingClient, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	switch cfg.Backend {
	case "llama_q8":
		return NewLlamaEmbedder(LlamaConfig{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Timeout:  cfg.Timeout,
		}), nil

	case "openai":
		return NewOpenAIEmbedder(OpenAIConfig{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Backend:  "openai",
			Timeout:  cfg.Timeout,
		}), nil

	case "openrouter":
		return NewOpenAIEmbedder(OpenAIConfig{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Backend:  "openrouter",
			Timeout:  cfg.Timeout,
		}), nil

	case "cohere":
		return NewCohereEmbedder(CohereConfig{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Timeout:  cfg.Timeout,
		}), nil

	case "voyage":
		return NewVoyageEmbedder(VoyageConfig{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Timeout:  cfg.Timeout,
		}), nil

	case "ollama":
		// Ollama exposes an OpenAI-compatible /v1/embeddings endpoint.
		// Reuses the OpenAIEmbedder with a different default base URL.
		endpoint := cfg.Endpoint
		if endpoint == "" {
			endpoint = "http://host.docker.internal:11434/v1"
		}
		return NewOpenAIEmbedder(OpenAIConfig{
			Endpoint: endpoint,
			APIKey:   cfg.APIKey, // optional for Ollama
			Model:    cfg.ModelName,
			Dim:      cfg.Dim,
			Backend:  "ollama",
			Timeout:  cfg.Timeout,
		}), nil

	default:
		return nil, fmt.Errorf("unknown embedding backend: %q (supported: llama_q8, openai, cohere, voyage, openrouter, ollama)", cfg.Backend)
	}
}

// SupportedBackends returns the list of supported backend identifiers.
func SupportedBackends() []string {
	return []string{"llama_q8", "openai", "cohere", "voyage", "openrouter", "ollama"}
}
