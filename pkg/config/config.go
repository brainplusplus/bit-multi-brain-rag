package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all application configuration loaded from environment / .env.
//
// Conventions:
//   - Secrets (API keys) are NEVER logged. Use SafeString() for display.
//   - All fields have a sensible default so the app can boot in dev mode
//     without a .env file, EXCEPT production-only fields (flagged below).
//   - Timeouts are in seconds unless suffixed.
type Config struct {
	// --- Server ---
	HTTPAddr    string // dashboard HTTP listen address (default ":8081")
	Environment string // "dev" | "production" (default "dev")

	// --- Auth (ADR-0003) ---
	// Comma-separated list of valid API keys. Global scope (1 key = all projects).
	// In production this MUST be set. In dev, defaults to a single dev key.
	DashboardAPIKeys []string

	// --- Embedding backend (ADR-0001) ---
	EmbeddingEndpoint  string // llama.cpp Q8 HTTP base URL (no trailing slash)
	EmbeddingModel     string // model name reported to client (e.g. "voyage-4-nano")
	EmbeddingDim       int    // vector dimension (1024 for voyage_nano_1024)
	EmbeddingAPIKey    string // API key for embedding server (LLAMA_API_KEY)
	EmbeddingTimeoutS  int    // HTTP timeout for embedding calls
	EmbeddingPooling   string // "mean" (voyage-4-nano requires mean, NOT cls)

	// --- Qdrant (vector store) ---
	QdrantURL    string // Qdrant HTTP endpoint (e.g. http://localhost:6333)
	QdrantAPIKey string // optional, empty if no auth

	// --- zvec (embedded vector store, zero-setup mode) ---
	// If ZvecPath is set, dashboard uses embedded zvec instead of Qdrant.
	// No Docker needed for vector storage.
	ZvecPath string // root directory for zvec data (e.g. "data/zvec")

	// --- Embedder binary (zero-setup mode) ---
	// If EmbedderBinary is set, dashboard starts llama-server as child process.
	// No Docker needed for embedding.
	EmbedderBinary string // path to llama-server binary
	EmbedderModel  string // path to GGUF model file
	EmbedderGPU    bool   // enable GPU layers

	// --- SQLite (project metadata store) ---
	DBPath string // path to SQLite database file (default "data/dashboard.db")

	// --- Index isolation key components (ADR-0001, ADR-0004) ---
	// These are the ACTIVE backend identifier embedded in collection names.
	// Collection naming: {project}_{domain}_{model}_{dim}_{backend}
	ActiveModel   string // e.g. "voyage_nano_1024"
	ActiveBackend string // e.g. "llama_q8"

	// --- MCP ---
	MCPEnabled bool // whether to start MCP server alongside dashboard
}

// Load reads configuration from environment variables.
// Call this once at startup.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:           getEnv("HTTP_ADDR", ":8081"),
		Environment:        getEnv("ENVIRONMENT", "dev"),
		DashboardAPIKeys:   getEnvSlice("DASHBOARD_API_KEYS", []string{"dev-local-key-change-me"}),
		EmbeddingEndpoint:  getEnv("EMBEDDING_ENDPOINT", "https://voyage.bitsolution.my.id"),
		EmbeddingModel:     getEnv("EMBEDDING_MODEL", "voyage-4-nano"),
		EmbeddingDim:       getEnvInt("EMBEDDING_DIM", 1024),
		EmbeddingAPIKey:    getEnv("EMBEDDING_API_KEY", ""),
		EmbeddingTimeoutS:  getEnvInt("EMBEDDING_TIMEOUT_S", 30),
		EmbeddingPooling:   getEnv("EMBEDDING_POOLING", "mean"),
		QdrantURL:          getEnv("QDRANT_URL", "http://localhost:6333"),
		QdrantAPIKey:       getEnv("QDRANT_API_KEY", ""),
		ZvecPath:           getEnv("ZVEC_PATH", ""),
		EmbedderBinary:     getEnv("EMBEDDER_BINARY", ""),
		EmbedderModel:      getEnv("EMBEDDER_MODEL", ""),
		EmbedderGPU:        getEnvBool("EMBEDDER_GPU", false),
		DBPath:             getEnv("DB_PATH", "data/dashboard.db"),
		ActiveModel:        getEnv("ACTIVE_MODEL", "voyage_nano_1024"),
		ActiveBackend:      getEnv("ACTIVE_BACKEND", "llama_q8"),
		MCPEnabled:         getEnvBool("MCP_ENABLED", true),
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate enforces production requirements.
func (c *Config) Validate() error {
	if c.Environment == "production" {
		if len(c.DashboardAPIKeys) == 0 || (len(c.DashboardAPIKeys) == 1 && c.DashboardAPIKeys[0] == "dev-local-key-change-me") {
			return fmt.Errorf("production requires DASHBOARD_API_KEYS to be set to strong random keys")
		}
		if c.EmbeddingAPIKey == "" {
			return fmt.Errorf("production requires EMBEDDING_API_KEY (LLAMA_API_KEY) to be set")
		}
	}
	if c.EmbeddingPooling != "mean" {
		// voyage-4-nano REQUIRES mean pooling. cls (server default) produces wrong embeddings.
		return fmt.Errorf("EMBEDDING_POOLING must be 'mean' for voyage-4-nano (got %q); wrong pooling = recall drop", c.EmbeddingPooling)
	}
	if c.EmbeddingDim <= 0 {
		return fmt.Errorf("EMBEDDING_DIM must be positive (got %d)", c.EmbeddingDim)
	}
	return nil
}

// SafeString returns a redacted summary for logging (never logs secrets).
func (c *Config) SafeString() string {
	return fmt.Sprintf(
		"Config{env=%s addr=%s embEndpoint=%s embModel=%s dim=%d pooling=%s "+
			"qdrant=%s activeModel=%s activeBackend=%s mcp=%v "+
			"apiKeys=%d embApiKey=%s}",
		c.Environment, c.HTTPAddr, c.EmbeddingEndpoint, c.EmbeddingModel,
		c.EmbeddingDim, c.EmbeddingPooling,
		c.QdrantURL, c.ActiveModel, c.ActiveBackend, c.MCPEnabled,
		len(c.DashboardAPIKeys), maskKey(c.EmbeddingAPIKey),
	)
}

// --- helpers ---

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvSlice(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// maskKey returns a redacted form of a secret for logging: first 4 + "..." + last 2.
func maskKey(k string) string {
	if k == "" {
		return "(unset)"
	}
	if len(k) <= 6 {
		return "***"
	}
	return k[:4] + "..." + k[len(k)-2:]
}

// IsValidAPIKey checks whether a provided key matches any configured key.
// Used by auth middleware (ADR-0003).
func (c *Config) IsValidAPIKey(key string) bool {
	for _, valid := range c.DashboardAPIKeys {
		if key == valid {
			return true
		}
	}
	return false
}
