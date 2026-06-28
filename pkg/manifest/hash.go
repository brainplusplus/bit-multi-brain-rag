package manifest

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
)

// hashFile computes FNV-1a 64-bit hash of file content, returned as hex string.
// FNV is fast (no crypto needed, just change detection) and collision-resistant
// enough for file dedup at this scale.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := fnv.New64a()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", h.Sum64()), nil
}
