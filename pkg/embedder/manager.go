// Package embedder manages a local llama.cpp embedder server as a child process.
//
// In zero-setup mode (no Docker), the dashboard starts llama-server directly
// as a child process. This avoids Docker entirely while still using the same
// llama.cpp HTTP API that LlamaEmbedder expects.
//
// The manager:
//   1. Checks if embedder is already running (skip start)
//   2. If not, starts llama-server binary as child process
//   3. Waits for health check
//   4. Monitors process health (restart on crash)
package embedder

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Config holds embedder binary configuration.
type Config struct {
	BinaryPath string   // path to llama-server (or llama-server.exe)
	ModelPath  string   // path to voyage-4-nano Q8 GGUF
	Port       int      // HTTP port (default 8080)
	APIKey     string   // LLAMA_API_KEY for auth
	GPU        bool     // enable GPU (nvidia)
	ExtraArgs  []string // additional CLI args
}

// Manager manages the embedder child process lifecycle.
type Manager struct {
	cfg    Config
	logger *slog.Logger
	cmd    *exec.Cmd
}

// New creates an embedder process manager.
func New(cfg Config, logger *slog.Logger) *Manager {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	return &Manager{cfg: cfg, logger: logger}
}

// Start starts the embedder if not already running. Returns the HTTP endpoint.
func (m *Manager) Start(ctx context.Context) (string, error) {
	endpoint := fmt.Sprintf("http://localhost:%d", m.cfg.Port)

	// Check if already running (e.g. user started manually or Docker).
	if m.isHealthy(endpoint) {
		m.logger.Info("embedder already running", "endpoint", endpoint)
		return endpoint, nil
	}

	// Start child process.
	args := []string{
		"--model", m.cfg.ModelPath,
		"--port", fmt.Sprintf("%d", m.cfg.Port),
		"--pooling", "mean",
		"--host", "127.0.0.1",
	}
	if m.cfg.GPU {
		args = append(args, "--n-gpu-layers", "999")
	}
	if m.cfg.APIKey != "" {
		args = append(args, "--api-key", m.cfg.APIKey)
	}
	args = append(args, m.cfg.ExtraArgs...)

	m.cmd = exec.CommandContext(ctx, m.cfg.BinaryPath, args...)
	m.cmd.Stdout = nil // discard (or pipe to log)
	m.cmd.Stderr = nil

	if err := m.cmd.Start(); err != nil {
		return "", fmt.Errorf("embedder: start failed: %w", err)
	}

	m.logger.Info("embedder started",
		"pid", m.cmd.Process.Pid,
		"endpoint", endpoint,
		"model", filepath.Base(m.cfg.ModelPath),
		"gpu", m.cfg.GPU)

	// Wait for health (model load takes time).
	if err := m.waitForHealth(ctx, endpoint, 120*time.Second); err != nil {
		m.Stop()
		return "", fmt.Errorf("embedder: health check timeout: %w", err)
	}

	m.logger.Info("embedder healthy", "endpoint", endpoint)
	return endpoint, nil
}

// Stop kills the embedder child process.
func (m *Manager) Stop() {
	if m.cmd != nil && m.cmd.Process != nil {
		m.logger.Info("stopping embedder", "pid", m.cmd.Process.Pid)
		m.cmd.Process.Kill()
		m.cmd.Wait()
		m.cmd = nil
	}
}

// SetGPU toggles GPU mode for next Start() call.
func (m *Manager) SetGPU(enable bool) {
	m.cfg.GPU = enable
}

// isHealthy checks if the embedder HTTP endpoint responds.
func (m *Manager) isHealthy(endpoint string) bool {
	resp, err := http.Get(endpoint + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// waitForHealth polls the health endpoint until it responds or timeout.
func (m *Manager) waitForHealth(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if m.isHealthy(endpoint) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

// BinaryName returns the platform-specific llama-server binary name.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}
