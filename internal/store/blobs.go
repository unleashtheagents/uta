package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Delete removes a blob file. Missing files are not an error — the desired
// post-state (absence) is already true, which matches the idempotent feel of
// a content-addressed store.
func (b *Blobs) Delete(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// BlobInfo describes a single stored blob: its absolute path, sha256 hex
// digest (the filename without extension), optional extension, and size in
// bytes.
type BlobInfo struct {
	Path string
	Hash string
	Ext  string
	Size int64
}

// List enumerates all blobs in the store. Entries that don't look like
// content-addressed blobs (e.g. leftover *.tmp files from interrupted Puts)
// are skipped, as are subdirectories. A missing blobs dir returns an empty
// slice rather than an error, mirroring the idempotent feel of Delete.
func (b *Blobs) List() ([]BlobInfo, error) {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]BlobInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		hash, ext, _ := strings.Cut(name, ".")
		if len(hash) != 64 || !isHex(hash) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out = append(out, BlobInfo{
			Path: filepath.Join(b.Dir, name),
			Hash: hash,
			Ext:  ext,
			Size: info.Size(),
		})
	}
	return out, nil
}

// UnreferencedBlobs compares the blobs on disk against every blob-ref
// column in the database (sessions.final_answer_ref, subtasks.prompt_ref,
// subtasks.raw_output_ref) and returns the orphans plus their total size in
// bytes. Detection only — callers decide whether to delete. Note refs are
// stored as absolute paths, so a relocated state dir makes every blob look
// orphaned; report before acting.
func (s *Store) UnreferencedBlobs(b *Blobs) ([]BlobInfo, int64, error) {
	refs := map[string]bool{}
	for _, q := range []string{
		`SELECT final_answer_ref FROM sessions WHERE final_answer_ref IS NOT NULL AND final_answer_ref != ''`,
		`SELECT prompt_ref FROM subtasks WHERE prompt_ref != ''`,
		`SELECT raw_output_ref FROM subtasks WHERE raw_output_ref IS NOT NULL AND raw_output_ref != ''`,
	} {
		rows, err := s.DB.Query(q)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				rows.Close()
				return nil, 0, err
			}
			refs[ref] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
	}

	blobs, err := b.List()
	if err != nil {
		return nil, 0, err
	}
	var orphans []BlobInfo
	var size int64
	for _, bi := range blobs {
		if !refs[bi.Path] {
			orphans = append(orphans, bi)
			size += bi.Size
		}
	}
	return orphans, size, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
