package indexer

import "os"

// readFile reads a file's contents into memory. Kept separate for testability
// (tests can inject an in-memory filesystem later).
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
