package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Blobs is a content-addressed file store under ~/.uta/blobs. Prompts, raw
// provider outputs, and any other large payloads are written here once and
// referenced by their sha256 hex digest.
type Blobs struct {
	Dir string
}

func NewBlobs(dir string) *Blobs { return &Blobs{Dir: dir} }

// Put writes data to a sha256-named file and returns the absolute path.
// Existing files with the same digest are a no-op (content-addressed).
func (b *Blobs) Put(data []byte, ext string) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	if ext != "" {
		name = name + "." + ext
	}
	full := filepath.Join(b.Dir, name)
	if _, err := os.Stat(full); err == nil {
		return full, nil
	}
	tmp, err := os.CreateTemp(b.Dir, "blob-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create blob tmp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return full, nil
}

// Get streams a blob's bytes to w.
func (b *Blobs) Get(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
