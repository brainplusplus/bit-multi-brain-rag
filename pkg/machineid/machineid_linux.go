//go:build linux

package machineid

import (
	"os"
	"strings"
)

func machineID() (string, error) {
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		id, err = os.ReadFile("/var/lib/dbus/machine-id")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(id)), nil
}

func platformHostname() (string, error) {
	return os.Hostname()
}
