package store

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestBlobsDelete_RemovesPutBlob(t *testing.T) {
	b := NewBlobs(t.TempDir())

	path, err := b.Put([]byte("hello"), "txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob should exist after Put: %v", err)
	}

	if err := b.Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blob should be gone after Delete, got err=%v", err)
	}

	var buf bytes.Buffer
	if err := b.Get(path, &buf); !os.IsNotExist(err) {
		t.Fatalf("Get on deleted blob: want IsNotExist, got %v", err)
	}
}

func TestBlobsDelete_MissingIsNoop(t *testing.T) {
	b := NewBlobs(t.TempDir())

	if err := b.Delete(b.Dir + "/does-not-exist"); err != nil {
		t.Fatalf("Delete missing should be a no-op, got: %v", err)
	}
}

func TestBlobsList_ReturnsPutBlobs(t *testing.T) {
	b := NewBlobs(t.TempDir())

	payloads := map[string][]byte{
		"txt":  []byte("hello"),
		"json": []byte(`{"a":1}`),
		"":     []byte("no extension here"),
	}
	wantPaths := make(map[string]int64, len(payloads))
	for ext, data := range payloads {
		path, err := b.Put(data, ext)
		if err != nil {
			t.Fatalf("Put(%q): %v", ext, err)
		}
		wantPaths[path] = int64(len(data))
	}

	got, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(payloads) {
		t.Fatalf("List returned %d entries, want %d (%+v)", len(got), len(payloads), got)
	}

	gotByPath := make(map[string]BlobInfo, len(got))
	for _, info := range got {
		gotByPath[info.Path] = info
		if len(info.Hash) != 64 {
			t.Errorf("hash %q is not a 64-char hex digest", info.Hash)
		}
	}
	for path, wantSize := range wantPaths {
		info, ok := gotByPath[path]
		if !ok {
			t.Errorf("List missing blob %s", path)
			continue
		}
		if info.Size != wantSize {
			t.Errorf("size for %s: got %d, want %d", path, info.Size, wantSize)
		}
	}
}

func TestBlobsList_EmptyDir(t *testing.T) {
	b := NewBlobs(t.TempDir())
	got, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List on empty dir: got %d entries, want 0", len(got))
	}
}

func TestBlobsList_MissingDir(t *testing.T) {
	b := NewBlobs(filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := b.List()
	if err != nil {
		t.Fatalf("List on missing dir should not error, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List on missing dir: got %d entries, want 0", len(got))
	}
}

func TestBlobsList_SkipsNonBlobFiles(t *testing.T) {
	b := NewBlobs(t.TempDir())

	if _, err := b.Put([]byte("real blob"), "txt"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A stray tmp file from an interrupted Put — should be ignored.
	if err := os.WriteFile(filepath.Join(b.Dir, "blob-12345.tmp"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("write stray tmp: %v", err)
	}
	// Something that just doesn't look like a content-addressed blob.
	if err := os.WriteFile(filepath.Join(b.Dir, "README"), []byte("not a blob"), 0o600); err != nil {
		t.Fatalf("write stray readme: %v", err)
	}
	// A subdirectory — should also be ignored.
	if err := os.Mkdir(filepath.Join(b.Dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		names := make([]string, len(got))
		for i, info := range got {
			names[i] = filepath.Base(info.Path)
		}
		sort.Strings(names)
		t.Fatalf("List returned %d entries (%v), want exactly 1 real blob", len(got), names)
	}
	if got[0].Ext != "txt" {
		t.Errorf("ext: got %q, want %q", got[0].Ext, "txt")
	}
	if got[0].Size != int64(len("real blob")) {
		t.Errorf("size: got %d, want %d", got[0].Size, len("real blob"))
	}
}

func TestBlobsList_DeletedBlobNotListed(t *testing.T) {
	b := NewBlobs(t.TempDir())

	path, err := b.Put([]byte("ephemeral"), "txt")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List after Delete: got %d entries, want 0", len(got))
	}

	var buf bytes.Buffer
	if err := b.Get(path, &buf); !os.IsNotExist(err) {
		t.Fatalf("Get on deleted blob: want IsNotExist, got %v", err)
	}
}
