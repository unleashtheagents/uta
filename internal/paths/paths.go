package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// Home returns the uta home directory. Honors $UTA_HOME, otherwise ~/.uta.
// The directory is created if missing.
func Home() (string, error) {
	if h := os.Getenv("UTA_HOME"); h != "" {
		if err := os.MkdirAll(h, 0o755); err != nil {
			return "", err
		}
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if u == "" {
		return "", errors.New("user home directory unknown")
	}
	p := filepath.Join(u, ".uta")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// DB returns the path to the SQLite database file.
func DB(home string) string { return filepath.Join(home, "uta.db") }

// Blobs returns the blob-store directory, creating it if needed.
func Blobs(home string) (string, error) {
	p := filepath.Join(home, "blobs")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// ProvidersDir returns the directory holding user-defined YAML provider descriptors.
func ProvidersDir(home string) (string, error) {
	p := filepath.Join(home, "providers")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}
