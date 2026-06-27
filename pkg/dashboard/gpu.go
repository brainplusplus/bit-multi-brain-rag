// Package dashboard — GPU detection and runtime switch (Phase 5).
//
// Architecture: the dashboard talks to the host docker daemon via a mounted
// docker socket (/var/run/docker.sock). It can:
//   - detect NVIDIA GPUs by reading /proc/driver/nvidia/version (mounted from host)
//     or by running nvidia-smi (if the binary is available in the container)
//   - check whether the nvidia-container-runtime is registered with docker
//   - stop/start the embedder container with the appropriate image+GPU runtime
//
// State persistence: the current mode (cpu | gpu) is stored in the settings
// table (key=embedder_mode). On startup the dashboard reads this and reports
// it as the current mode; it does NOT auto-recreate the embedder — that
// happens only when the user explicitly clicks Switch.
package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// GPUStatus is the JSON shape returned by /api/v1/gpu/status.
type GPUStatus struct {
	Detected         bool   `json:"detected"`           // GPU present on host
	Vendor           string `json:"vendor"`             // "nvidia" (only one supported)
	Name             string `json:"name"`               // e.g. "NVIDIA GeForce RTX 3090"
	VRAMTotalMB      int    `json:"vram_total_mb"`      // 0 if unknown
	VRAMUsedMB       int    `json:"vram_used_mb"`       // 0 if unknown
	DriverVersion    string `json:"driver_version"`     // e.g. "535.183.01"
	ContainerToolkit bool   `json:"container_toolkit"`  // nvidia-container-runtime registered with docker
	CDIDevices       int    `json:"cdi_devices"`        // number of CDI devices discovered by docker (>=1 means CDI spec loaded)
	HostType         string `json:"host_type"`          // "rancher-desktop" | "docker-desktop" | "linux" | "unknown"
	CurrentMode      string `json:"current_mode"`       // "cpu" | "gpu" (from settings)
	SwitchInProgress bool   `json:"switch_in_progress"` // true if a switch is running
	LastError        string `json:"last_error,omitempty"`
	// Embedder runtime info (best-effort)
	EmbedderImage  string `json:"embedder_image,omitempty"`
	EmbedderStatus string `json:"embedder_status,omitempty"`
}

// HealthIssue summarizes a single problem with the GPU/toolkit setup.
type HealthIssue struct {
	Severity string // "warn" | "error"
	Title    string
	Detail   string
	Fix      string // human readable recovery command (rendered as <code>)
}

// HealthIssues computes a list of actionable problems based on the status.
func (g GPUStatus) HealthIssues() []HealthIssue {
	var out []HealthIssue
	// Case 1: GPU detected but no docker runtime
	if g.Detected && !g.ContainerToolkit {
		fix := "Install NVIDIA Container Toolkit and configure dockerd."
		if g.HostType == "rancher-desktop" {
			fix = "wsl -d rancher-desktop -- sh /mnt/d/golang/bit-rag/bit-multi-brain-rag/scripts/rancher-nvidia-install.sh"
		}
		out = append(out, HealthIssue{
			Severity: "error",
			Title:    "GPU detected, but Docker runtime missing",
			Detail:   "nvidia-container-runtime is not registered with the Docker daemon. GPU mode switch will fail until this is fixed.",
			Fix:      fix,
		})
	}
	// Case 2: Toolkit registered but no CDI devices (stale spec, e.g. after driver update)
	if g.ContainerToolkit && g.CDIDevices == 0 && g.HostType == "rancher-desktop" {
		out = append(out, HealthIssue{
			Severity: "warn",
			Title:    "CDI spec missing or stale",
			Detail:   "Docker has nvidia runtime, but no CDI devices are discovered. This usually means /var/run/cdi/nvidia.yaml was lost (tmpfs wipe) or the driver path changed (Windows driver update).",
			Fix:      "wsl -d rancher-desktop -- /usr/local/bin/nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml",
		})
	}
	// Case 3: Toolkit registered, no CDI on non-Rancher host
	if g.ContainerToolkit && g.CDIDevices == 0 && g.HostType != "rancher-desktop" && g.Detected {
		out = append(out, HealthIssue{
			Severity: "warn",
			Title:    "No CDI devices discovered",
			Detail:   "Docker reports the nvidia runtime is registered, but no CDI devices are visible. The legacy --gpus mode may still work, but CDI is recommended for newer setups.",
			Fix:      "sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml",
		})
	}
	return out
}

// detectHostType returns a coarse identifier for where the docker daemon lives.
// Detection is best-effort and runs from inside the dashboard container by
// inspecting docker info Name / OperatingSystem.
func detectHostType(ctx context.Context) string {
	resp, err := dockerAPIGet(ctx, "/v1.43/info")
	if err != nil {
		return "unknown"
	}
	var info struct {
		Name            string `json:"Name"`
		OperatingSystem string `json:"OperatingSystem"`
		OSType          string `json:"OSType"`
	}
	if err := json.Unmarshal(resp, &info); err != nil {
		return "unknown"
	}
	low := strings.ToLower(info.OperatingSystem + " " + info.Name)
	switch {
	case strings.Contains(low, "rancher"):
		return "rancher-desktop"
	case strings.Contains(low, "docker desktop"):
		return "docker-desktop"
	case info.OSType == "linux":
		return "linux"
	}
	return "unknown"
}

// countCDIDevices probes Docker info for CDI device count. Docker's HTTP API
// (as of v1.43) only exposes CDISpecDirs and not DiscoveredDevices, so we use
// two signals:
//   1. CDISpecDirs is non-empty (CDI feature is on)
//   2. At least one .yaml or .json file exists in one of those dirs
//      (we can probe via a tiny container exec, but that's heavy)
//
// As a pragmatic compromise, we return 1 if CDI is configured AND nvidia
// runtime is present (the spec is the only way nvidia runtime becomes useful
// on a Rancher-style host). Returns 0 otherwise.
func countCDIDevices(ctx context.Context) int {
	resp, err := dockerAPIGet(ctx, "/v1.43/info")
	if err != nil {
		return 0
	}
	var info struct {
		CDISpecDirs []string       `json:"CDISpecDirs"`
		Runtimes    map[string]any `json:"Runtimes"`
		// DiscoveredDevices may appear in newer Docker versions (>= 26).
		DiscoveredDevices []struct {
			Source string `json:"Source"`
			ID     string `json:"ID"`
		} `json:"DiscoveredDevices"`
	}
	if err := json.Unmarshal(resp, &info); err != nil {
		return 0
	}
	// Newer Docker: count CDI sources directly.
	if len(info.DiscoveredDevices) > 0 {
		n := 0
		for _, d := range info.DiscoveredDevices {
			if strings.EqualFold(d.Source, "cdi") {
				n++
			}
		}
		if n > 0 {
			return n
		}
	}
	// Older Docker (< 26): infer from CDISpecDirs + nvidia runtime presence.
	hasCDI := len(info.CDISpecDirs) > 0
	_, hasNvidia := info.Runtimes["nvidia"]
	if hasCDI && hasNvidia {
		return 1
	}
	return 0
}

// gpuMu serializes switch operations (only one switch at a time).
var gpuMu sync.Mutex
var gpuSwitchInProgress bool

// detectGPU probes the host for NVIDIA GPU presence. Best-effort; all probes
// are designed to fail silently and return Detected=false rather than error.
func detectGPU(ctx context.Context) GPUStatus {
	st := GPUStatus{Vendor: "nvidia"}
	// Probe 1: nvidia-smi binary (most reliable when available)
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		cmd := exec.CommandContext(ctx, path,
			"--query-gpu=name,memory.total,memory.used,driver_version",
			"--format=csv,noheader,nounits")
		out, err := cmd.Output()
		if err == nil {
			line := strings.TrimSpace(string(bytes.SplitN(out, []byte{'\n'}, 2)[0]))
			parts := strings.Split(line, ",")
			if len(parts) >= 4 {
				st.Detected = true
				st.Name = strings.TrimSpace(parts[0])
				st.VRAMTotalMB, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
				st.VRAMUsedMB, _ = strconv.Atoi(strings.TrimSpace(parts[2]))
				st.DriverVersion = strings.TrimSpace(parts[3])
			}
		}
	}
	// Probe 2: fallback to /proc/driver/nvidia/version (host-mounted)
	if !st.Detected {
		if data, err := os.ReadFile("/proc/driver/nvidia/version"); err == nil {
			st.Detected = true
			// Parse driver version from "NVRM version: NVIDIA UNIX x86_64 Kernel Module  535.183.01  ..."
			scanner := bufio.NewScanner(bytes.NewReader(data))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "Kernel Module") {
					fields := strings.Fields(line)
					for i := range fields {
						if i+1 < len(fields) && strings.HasSuffix(fields[i], "Module") {
							st.DriverVersion = fields[i+1]
							break
						}
					}
				}
			}
		}
	}
	// Probe 3: check docker info for nvidia runtime + host type + CDI devices.
	// We make a single docker info call here and reuse the parse below.
	st.ContainerToolkit = checkNvidiaRuntime(ctx)
	st.HostType = detectHostType(ctx)
	st.CDIDevices = countCDIDevices(ctx)

	// Probe 4: when the dashboard runs inside a container (no nvidia-smi, no
	// /proc/driver/nvidia mount), but the host has CDI devices + nvidia
	// runtime registered, we KNOW a GPU is accessible to docker on the host.
	// Mark Detected=true so the user can switch the embedder to GPU mode.
	// Name/VRAM/Driver remain empty here — they'll be populated once the GPU
	// embedder container starts and runs nvidia-smi.
	if !st.Detected && st.ContainerToolkit && st.CDIDevices > 0 {
		st.Detected = true
		if st.Name == "" {
			st.Name = "GPU available (via CDI)"
		}
	}
	return st
}

// checkNvidiaRuntime returns true if docker daemon has nvidia runtime registered.
// We query the local docker socket via HTTP.
func checkNvidiaRuntime(ctx context.Context) bool {
	resp, err := dockerAPIGet(ctx, "/v1.43/info")
	if err != nil {
		return false
	}
	var info struct {
		Runtimes map[string]any `json:"Runtimes"`
	}
	if err := json.Unmarshal(resp, &info); err != nil {
		return false
	}
	_, ok := info.Runtimes["nvidia"]
	return ok
}

// dockerAPIGet sends a GET to the docker socket and returns the body.
func dockerAPIGet(ctx context.Context, path string) ([]byte, error) {
	socketPath := "/var/run/docker.sock"
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("docker socket not mounted")
	}
	return dockerSocketGet(ctx, socketPath, path)
}

// dockerSocketGet is a minimal HTTP-over-unix-socket GET implementation.
// We use the proper net.Dialer rather than fight net/http's Transport.
func dockerSocketGet(ctx context.Context, socketPath, urlPath string) ([]byte, error) {
	conn, err := dialUnixNet(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := fmt.Sprintf("GET %s HTTP/1.0\r\nHost: docker\r\nAccept: application/json\r\n\r\n", urlPath)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	all, err := readAll(conn)
	if err != nil {
		return nil, err
	}
	// Split status line + headers from body.
	idx := bytes.Index(all, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, errors.New("invalid http response")
	}
	headers := string(all[:idx])
	body := all[idx+4:]
	if !strings.HasPrefix(headers, "HTTP/1.0 200") && !strings.HasPrefix(headers, "HTTP/1.1 200") {
		return nil, fmt.Errorf("docker api: %s", strings.SplitN(headers, "\r\n", 2)[0])
	}
	return body, nil
}

// dockerSocketPost sends a POST with optional body.
func dockerSocketPost(ctx context.Context, socketPath, urlPath string, body []byte) ([]byte, error) {
	conn, err := dialUnixNet(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := fmt.Sprintf("POST %s HTTP/1.0\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", urlPath, len(body))
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return nil, err
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second)) // pull can take long
	all, err := readAll(conn)
	if err != nil {
		return nil, err
	}
	idx := bytes.Index(all, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, errors.New("invalid http response")
	}
	headers := string(all[:idx])
	respBody := all[idx+4:]
	if !strings.HasPrefix(headers, "HTTP/1.0 2") && !strings.HasPrefix(headers, "HTTP/1.1 2") {
		return respBody, fmt.Errorf("docker api: %s — %s", strings.SplitN(headers, "\r\n", 2)[0], string(respBody))
	}
	return respBody, nil
}

// embedderContainerInfo returns image + status of the bit-rag-embedder container.
func embedderContainerInfo(ctx context.Context) (image, status string) {
	body, err := dockerSocketGet(ctx, "/var/run/docker.sock",
		"/v1.43/containers/bit-rag-embedder/json")
	if err != nil {
		return "", ""
	}
	var info struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", ""
	}
	return info.Config.Image, info.State.Status
}

// --- Switch logic ---

// switchRequest is the JSON body for POST /api/v1/gpu/switch.
type switchRequest struct {
	Mode string `json:"mode"` // "gpu" | "cpu"
}

// switchResult is returned by both API + UI handlers.
type switchResult struct {
	OK       bool   `json:"ok"`
	Mode     string `json:"mode"`
	Message  string `json:"message"`
	Steps    []step `json:"steps"`
	Rollback bool   `json:"rollback,omitempty"`
}

type step struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // pending | running | done | failed | skipped
	Duration int64  `json:"duration_ms"`
	Detail   string `json:"detail,omitempty"`
}

// performSwitch is the synchronous state machine for mode change.
// Caller must hold gpuMu.
func (s *Server) performSwitch(ctx context.Context, targetMode string) switchResult {
	res := switchResult{Mode: targetMode}
	currentMode := s.store.GetSetting(ctx, "embedder_mode")
	if currentMode == targetMode {
		res.OK = true
		res.Message = "Already in " + targetMode + " mode"
		return res
	}
	addStep := func(name string, fn func() error) bool {
		start := time.Now()
		st := step{Name: name, Status: "running"}
		err := fn()
		st.Duration = time.Since(start).Milliseconds()
		if err != nil {
			st.Status = "failed"
			st.Detail = err.Error()
			res.Steps = append(res.Steps, st)
			res.Message = "Failed at: " + name + ": " + err.Error()
			return false
		}
		st.Status = "done"
		res.Steps = append(res.Steps, st)
		return true
	}

	targetImage := "bit-rag-embedder:cpu"
	useGPU := false
	if targetMode == "gpu" {
		targetImage = "bit-rag-embedder:gpu"
		useGPU = true
	}

	// 1. Pre-flight
	if !addStep("pre-flight", func() error {
		if targetMode == "gpu" {
			st := detectGPU(ctx)
			if !st.Detected {
				return errors.New("no NVIDIA GPU detected on host")
			}
			if !st.ContainerToolkit {
				return errors.New("nvidia-container-runtime not registered with docker (install NVIDIA Container Toolkit)")
			}
			if st.VRAMTotalMB > 0 && st.VRAMTotalMB < 2000 {
				return fmt.Errorf("insufficient VRAM: %d MB (need at least 2 GB)", st.VRAMTotalMB)
			}
			if st.CDIDevices == 0 && (st.HostType == "rancher-desktop" || st.HostType == "linux") {
				return fmt.Errorf("no CDI devices discovered — regenerate spec first: "+
					"on Rancher Desktop run `wsl -d rancher-desktop -- /usr/local/bin/nvidia-ctk cdi generate "+
					"--output=/var/run/cdi/nvidia.yaml`; on Linux run "+
					"`sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`")
			}
		}
		return nil
	}) {
		return res
	}

	// 2. Pull image
	if !addStep("pull image "+targetImage, func() error {
		return dockerPullImage(ctx, targetImage)
	}) {
		return res
	}

	// 3. Stop existing embedder
	addStep("stop existing embedder", func() error {
		_ = dockerStopContainer(ctx, "bit-rag-embedder")
		return nil // best-effort
	})

	// 4. Remove existing
	addStep("remove existing container", func() error {
		_ = dockerRemoveContainer(ctx, "bit-rag-embedder")
		return nil
	})

	// 5. Start new embedder
	if !addStep("start "+targetImage, func() error {
		return dockerStartEmbedder(ctx, targetImage, useGPU)
	}) {
		// Try rollback to previous mode
		res.Rollback = true
		fallbackImage := "bit-rag-embedder:cpu"
		_ = dockerStopContainer(ctx, "bit-rag-embedder")
		_ = dockerRemoveContainer(ctx, "bit-rag-embedder")
		_ = dockerStartEmbedder(ctx, fallbackImage, false)
		return res
	}

	// 6. Health probe
	if !addStep("health probe", func() error {
		return embedderHealthProbe(ctx, 60*time.Second)
	}) {
		res.Rollback = true
		// rollback
		_ = dockerStopContainer(ctx, "bit-rag-embedder")
		_ = dockerRemoveContainer(ctx, "bit-rag-embedder")
		_ = dockerStartEmbedder(ctx, "bit-rag-embedder:cpu", false)
		_ = s.store.SetSetting(ctx, "embedder_mode", "cpu")
		return res
	}

	// 7. Persist mode
	addStep("persist mode", func() error {
		return s.store.SetSetting(ctx, "embedder_mode", targetMode)
	})

	res.OK = true
	res.Message = "Switched to " + targetMode + " mode"
	return res
}

// --- HTTP handlers ---

func (s *Server) apiGPUStatus(c echo.Context) error {
	st := detectGPU(c.Request().Context())
	st.CurrentMode = s.store.GetSetting(c.Request().Context(), "embedder_mode")
	if st.CurrentMode == "" {
		st.CurrentMode = "cpu"
	}
	st.SwitchInProgress = gpuSwitchInProgress
	st.EmbedderImage, st.EmbedderStatus = embedderContainerInfo(c.Request().Context())
	return c.JSON(200, st)
}

func (s *Server) apiGPUSwitch(c echo.Context) error {
	var req switchRequest
	if err := c.Bind(&req); err != nil || (req.Mode != "gpu" && req.Mode != "cpu") {
		return c.JSON(400, map[string]string{"error": "mode must be 'gpu' or 'cpu'"})
	}
	if !gpuMu.TryLock() {
		return c.JSON(409, map[string]string{"error": "another switch in progress"})
	}
	gpuSwitchInProgress = true
	defer func() {
		gpuSwitchInProgress = false
		gpuMu.Unlock()
	}()
	res := s.performSwitch(c.Request().Context(), req.Mode)
	if !res.OK {
		return c.JSON(500, res)
	}
	return c.JSON(200, res)
}
