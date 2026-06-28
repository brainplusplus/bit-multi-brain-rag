// Package machineid provides a stable, per-machine identifier without
// requiring any special privileges. Based on github.com/zeroshade/machine-id
// (Apache-2.0), which itself is based on github.com/denisbrodbeck/machineid.
//
// Platform sources:
//   Windows: HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid (registry)
//   Linux:   /etc/machine-id or /var/lib/dbus/machine-id
//   macOS:   IOPlatformUUID (ioreg)
//   BSD:     kenv smbios.system.uuid (FreeBSD) or /etc/hostid
package machineid

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"runtime"
)

// ID returns the platform-specific machine ID. The ID is stable for the
// OS installation and survives updates and hardware changes.
func ID() (string, error) {
	id, err := machineID()
	if err != nil {
		return "", fmt.Errorf("machineid: %w", err)
	}
	return id, nil
}

// ProtectedID returns an HMAC-SHA256 hash of the machine ID using a fixed
// app-specific key. This is privacy-preserving: the raw hardware/system
// ID never leaves the machine, and the hash is deterministic per machine.
func ProtectedID() (string, error) {
	id, err := ID()
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, []byte("bit-rag-v1"))
	h.Write([]byte(id))
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Hostname returns the machine's hostname (for display purposes).
func Hostname() string {
	host, _ := platformHostname()
	return host
}

// OS returns the runtime.GOOS string (e.g. "windows", "linux", "darwin").
func OS() string {
	return runtime.GOOS
}
