//go:build darwin

package machineid

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func machineID() (string, error) {
	out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "IOPlatformUUID") {
			parts := strings.SplitN(line, "\"", 4)
			if len(parts) >= 4 {
				return strings.TrimSpace(parts[3]), nil
			}
		}
	}
	return "", fmt.Errorf("machineid: IOPlatformUUID not found")
}

func platformHostname() (string, error) {
	return os.Hostname()
}
