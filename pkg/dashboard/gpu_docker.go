package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// dialUnix returns the same connection type used by net.Dial but typed as
// our interface placeholder. Kept for compile parity with gpu.go; main
// implementation uses dialUnixNet.
func dialUnix(ctx context.Context, socketPath string) (net.Conn, error) {
	return dialUnixNet(ctx, socketPath)
}

// dialUnixNet opens a unix socket connection with context deadline support.
func dialUnixNet(ctx context.Context, socketPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, "unix", socketPath)
}

// readAll reads everything from the connection up to a 4MB cap (defensive).
func readAll(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return buf, err
		}
		if len(buf) > 4<<20 {
			break
		}
	}
	return buf, nil
}

// dockerPullImage performs POST /v1.43/images/create?fromImage=...&tag=...
func dockerPullImage(ctx context.Context, image string) error {
	name, tag := splitImageTag(image)
	path := fmt.Sprintf("/v1.43/images/create?fromImage=%s&tag=%s", name, tag)
	body, err := dockerSocketPost(ctx, "/var/run/docker.sock", path, nil)
	if err != nil {
		// If image already exists locally we'll still get success; surface other errors
		return fmt.Errorf("pull failed: %w (body=%s)", err, truncate(string(body), 200))
	}
	return nil
}

// dockerStopContainer issues POST /containers/{name}/stop
func dockerStopContainer(ctx context.Context, name string) error {
	_, err := dockerSocketPost(ctx, "/var/run/docker.sock",
		"/v1.43/containers/"+name+"/stop?t=10", nil)
	return err
}

// dockerRemoveContainer issues DELETE /containers/{name}?force=1
func dockerRemoveContainer(ctx context.Context, name string) error {
	// DELETE via raw socket
	conn, err := dialUnixNet(ctx, "/var/run/docker.sock")
	if err != nil {
		return err
	}
	defer conn.Close()
	req := fmt.Sprintf("DELETE /v1.43/containers/%s?force=1 HTTP/1.0\r\nHost: docker\r\n\r\n", name)
	_, _ = conn.Write([]byte(req))
	_, _ = readAll(conn)
	return nil // best-effort
}

// dockerStartEmbedder creates + starts a new bit-rag-embedder container with
// the given image, optionally with NVIDIA GPU access.
func dockerStartEmbedder(ctx context.Context, image string, useGPU bool) error {
	// Create container payload (subset of Docker create API).
	type endpoints struct {
		// Empty placeholder for future bridge config
	}
	type deviceRequest struct {
		Driver       string     `json:"Driver"`
		Count        int        `json:"Count"`
		Capabilities [][]string `json:"Capabilities"`
	}
	type hostConfig struct {
		NetworkMode    string          `json:"NetworkMode"`
		RestartPolicy  map[string]any  `json:"RestartPolicy"`
		DeviceRequests []deviceRequest `json:"DeviceRequests,omitempty"`
		Runtime        string          `json:"Runtime,omitempty"`
	}
	type createReq struct {
		Image      string            `json:"Image"`
		Cmd        []string          `json:"Cmd,omitempty"`
		Env        []string          `json:"Env,omitempty"`
		ExposedPorts map[string]any  `json:"ExposedPorts,omitempty"`
		HostConfig hostConfig        `json:"HostConfig"`
	}
	cr := createReq{
		Image: image,
		HostConfig: hostConfig{
			NetworkMode:   "bit-rag_default",
			RestartPolicy: map[string]any{"Name": "unless-stopped"},
		},
	}
	if useGPU {
		cr.HostConfig.DeviceRequests = []deviceRequest{{
			Driver:       "nvidia",
			Count:        -1, // all GPUs
			Capabilities: [][]string{{"gpu"}},
		}}
	}
	payload, _ := json.Marshal(cr)
	body, err := dockerSocketPost(ctx, "/var/run/docker.sock",
		"/v1.43/containers/create?name=bit-rag-embedder", payload)
	if err != nil {
		return fmt.Errorf("container create: %w (%s)", err, truncate(string(body), 300))
	}
	// Start it
	_, err = dockerSocketPost(ctx, "/var/run/docker.sock",
		"/v1.43/containers/bit-rag-embedder/start", nil)
	if err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	return nil
}

// embedderHealthProbe polls the embedder's /health endpoint until success or timeout.
func embedderHealthProbe(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	cli := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://bit-rag-embedder:8080/health", nil)
		resp, err := cli.Do(req)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = errors.New("health probe timed out")
	}
	return lastErr
}

// splitImageTag splits "name:tag" → ("name", "tag"); defaults tag to "latest".
func splitImageTag(image string) (string, string) {
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			return image[:i], image[i+1:]
		}
		if image[i] == '/' {
			break
		}
	}
	return image, "latest"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Compile-time guard so unused-import warnings don't bite if we trim helpers later.
var _ = bytes.Equal
