// Package paths is the single source of truth for where uta keeps its state
// on disk. There are two distinct roots:
//
//   - Global home (~/.uta or $UTA_HOME): houses the registry of declarative
//     provider descriptors and the fallback DB/blob store when the user is
//     not inside a project.
//
//   - Project state (.uta/ inside a project root): houses the project-scoped
//     DB, blobs, and the shared context directory. Created by
//     `uta project init`.
//
// `uta` auto-detects whether the cwd is inside a project by walking up the
// filesystem looking for a `.uta/` directory.
package paths

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProjectStateName is the directory uta creates inside a project root.
const ProjectStateName = ".uta"

// Home returns the GLOBAL uta home directory. Honors $UTA_HOME, otherwise
// ~/.uta. The directory is created if missing. This is always the same
// regardless of cwd — used for provider descriptors and fallback state.
func Home() (string, error) {
	if h := os.Getenv("UTA_HOME"); h != "" {
		if err := os.MkdirAll(h, 0o755); err != nil {
			return "", err
		}
		return h, nil
	}
	u, err := UserHome()
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

// UserHome is the user's home directory. HOME wins when set, on every OS,
// so Git Bash on Windows, containers and tests that point HOME somewhere
// get the same answer as the shell; otherwise os.UserHomeDir decides
// (USERPROFILE on Windows).
func UserHome() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// DB returns the path to the SQLite database file inside any state dir.
func DB(stateDir string) string { return filepath.Join(stateDir, "uta.db") }

// Blobs returns the blob-store directory inside a state dir, creating it if
// needed.
func Blobs(stateDir string) (string, error) {
	p := filepath.Join(stateDir, "blobs")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// ProvidersDir returns the directory holding user-defined YAML provider
// descriptors. Always lives under the GLOBAL home, never under a project —
// providers are user-wide.
func ProvidersDir(globalHome string) (string, error) {
	p := filepath.Join(globalHome, "providers")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// FindProjectRoot walks up the filesystem from start looking for a directory
// containing a `.uta` subdirectory. Returns (root, true) if found, ("", false)
// if not. Stops at the filesystem root or the user's home dir, whichever
// comes first.
func FindProjectRoot(start string) (string, bool) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", false
		}
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	homeDir, _ := UserHome()
	for {
		// Don't conflate the global ~/.uta with a project state dir. If we're
		// AT the user's home directory and a .uta exists there, treat that
		// as the global home, not a project root.
		if abs == homeDir {
			return "", false
		}
		candidate := filepath.Join(abs, ProjectStateName)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return abs, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false // reached fs root
		}
		abs = parent
	}
}

// ProjectStateDir returns the .uta/ state dir inside a project root.
func ProjectStateDir(projectRoot string) string {
	return filepath.Join(projectRoot, ProjectStateName)
}

// ProjectContextDir returns the shared-context dir inside a project, creating
// it if needed.
func ProjectContextDir(projectRoot string) (string, error) {
	p := filepath.Join(projectRoot, ProjectStateName, "context")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// WriteFileAtomic writes data to path by first writing to a sibling temp file
// and then renaming it into place. This ensures readers never observe a
// partially-written file even if the process is interrupted mid-write.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// renameWithRetry replaces path with tmp. On Windows a reader that has the
// target open makes the rename fail with a sharing violation for a few
// milliseconds; a handful of short retries hides that without changing the
// atomic-replace semantics anywhere else.
func renameWithRetry(tmp, path string) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(5+attempt*5) * time.Millisecond)
	}
	return err
}

// CredentialsFile returns the path to the user-level credentials file inside
// the global ~/.uta home directory (~/.uta/credentials.env). Keeping API keys
// here ensures they live outside every git repository with 0600 permissions.
func CredentialsFile(globalHome string) string {
	return filepath.Join(globalHome, "credentials.env")
}

// LoadCredentials reads ~/.uta/credentials.env (if present) and populates any
// environment variables that are not already defined in the current process.
// Supports both `KEY=value` and `export KEY="value"` lines.
func LoadCredentials() {
	home, err := Home()
	if err != nil {
		return
	}
	_ = LoadCredentialsFromFile(CredentialsFile(home))
}

// LoadCredentialsFromFile parses a dotenv/shell file and sets any environment
// variables not already present in os.LookupEnv.
func LoadCredentialsFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if k == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return scanner.Err()
}
