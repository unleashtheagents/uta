package improve

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RetrospectiveDir returns the canonical directory for per-mode retrospective
// markdown files within a project. Returns "" when projectRoot is empty.
func RetrospectiveDir(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	return filepath.Join(projectRoot, ".uta", "context", "retrospectives")
}

// RetrospectiveFile is one on-disk retrospective markdown discovered by
// ListRetrospectives. Mode is parsed from the filename prefix (everything
// before the final "-YYYYMMDD" component); it stays "" when the file does
// not follow the naming convention.
type RetrospectiveFile struct {
	Path    string
	Name    string
	Mode    string
	ModTime time.Time
}

// ListRetrospectives returns every *.md file under
// <projectRoot>/.uta/context/retrospectives/, sorted by mod time (newest
// first). A missing directory is not an error (returns nil, nil) so callers
// can use this safely on fresh projects.
func ListRetrospectives(projectRoot string) ([]RetrospectiveFile, error) {
	dir := RetrospectiveDir(projectRoot)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]RetrospectiveFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, RetrospectiveFile{
			Path:    filepath.Join(dir, name),
			Name:    name,
			Mode:    parseRetroMode(name),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// parseRetroMode extracts the mode prefix from a "<mode>-<YYYYMMDD>.md" or
// "<mode>-<YYYYMMDD>-<N>.md" filename. Returns "" when the filename does not
// follow either convention, preserving graceful behavior on hand-dropped files.
func parseRetroMode(filename string) string {
	stem := strings.TrimSuffix(filename, ".md")
	if stem == filename {
		return ""
	}
	idx := strings.LastIndex(stem, "-")
	if idx <= 0 {
		return ""
	}
	tail := stem[idx+1:]
	if !allDigits(tail) {
		return ""
	}
	if len(tail) == 8 {
		return stem[:idx]
	}
	// Counter form: trailing N, previous component must be the 8-digit date.
	head := stem[:idx]
	idx2 := strings.LastIndex(head, "-")
	if idx2 <= 0 {
		return ""
	}
	date := head[idx2+1:]
	if len(date) != 8 || !allDigits(date) {
		return ""
	}
	return head[:idx2]
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// uniqueRetroPath returns a non-colliding path under dir for the given mode
// and date. The first call of the day produces "<mode>-<date>.md"; subsequent
// calls produce "<mode>-<date>-2.md", "-3.md", and so on. Hits up to 1000
// before surfacing an error — well above any plausible per-day cadence.
func uniqueRetroPath(dir, mode, date string) (string, error) {
	base := filepath.Join(dir, mode+"-"+date+".md")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base, nil
	} else if err != nil {
		return "", err
	}
	for n := 2; n <= 1000; n++ {
		path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.md", mode, date, n))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("retrospective: > 1000 retros for %s on %s", mode, date)
}
