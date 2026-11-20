package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Subject412/dubarr/backend/internal/settings"
)

var errPathEscaped = errors.New("api: path escaped the configured storage roots")

type browseEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

type browseResponse struct {
	Path    string        `json:"path"`
	Parent  *string       `json:"parent"`
	Entries []browseEntry `json:"entries"`
}

// handleBrowse is the setup wizard's directory picker.
//
// Canonicalization: the requested path must be absolute. It is cleaned and
// resolved with filepath.EvalSymlinks before any access or comparison, so ".."
// segments and symlinks cannot address anything outside the configured roots.
//
// Roots: the configured storage.paths values (watch_dir, vault_dir, work_dir and
// media_roots), themselves EvalSymlinks-resolved. While the wizard is still
// unconfigured (no roots yet), browsing any absolute existing directory is
// allowed — otherwise the first-run wizard could never pick a root. That mode is
// admin-only and is the documented exception. Once at least one root exists, the
// resolved path must be equal to or inside a root.
//
// Errors never echo a server path: a traversal attempt and a missing directory
// produce the same shape of message apart from their status code.
func (s *Service) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store is not configured")
		return
	}
	roots, err := s.browseRoots(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read storage roots")
		return
	}

	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested == "" && len(roots) > 0 {
		requested = roots[0]
	}
	if requested == "" {
		requested = string(filepath.Separator)
	}
	if strings.ContainsRune(requested, '\x00') {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	clean := filepath.Clean(requested)
	if !filepath.IsAbs(clean) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, "directory not found")
		case errors.Is(err, os.ErrPermission):
			writeError(w, http.StatusForbidden, "directory is not accessible")
		default:
			writeError(w, http.StatusBadRequest, "path cannot be resolved")
		}
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, "directory not found")
		case errors.Is(err, os.ErrPermission):
			writeError(w, http.StatusForbidden, "directory is not accessible")
		default:
			writeError(w, http.StatusBadRequest, "path cannot be resolved")
		}
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is not a directory")
		return
	}
	if len(roots) > 0 && !withinAny(roots, resolved) {
		writeError(w, http.StatusForbidden, "path is outside the configured storage roots")
		return
	}

	entries, err := s.listDir(resolved, roots)
	if err != nil {
		writeError(w, http.StatusForbidden, "directory is not accessible")
		return
	}

	resp := browseResponse{Path: resolved, Entries: entries}
	if resolved != string(filepath.Separator) {
		parent := filepath.Dir(resolved)
		if len(roots) == 0 || withinAny(roots, parent) {
			resp.Parent = &parent
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) browseRoots(r *http.Request) ([]string, error) {
	raw, ok, err := s.store.Get(r.Context(), settings.KeyStoragePaths)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var paths settings.StoragePaths
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, err
	}
	candidates := append([]string{paths.WatchDir, paths.VaultDir, paths.WorkDir}, paths.MediaRoots...)
	roots := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue // not created yet: unusable as a browse root
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		roots = append(roots, resolved)
	}
	return roots, nil
}

func (s *Service) listDir(dir string, roots []string) ([]browseEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// TOCTOU guard: dir was EvalSymlinks-resolved before the open, but the path
	// could have been swapped for a symlink in between. Re-resolve it now and
	// require the very same real directory, still inside the roots. Portable on
	// purpose (no openat2), so Windows keeps working.
	if recheck, err := filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	} else if recheck != dir {
		return nil, errPathEscaped
	} else if len(roots) > 0 && !withinAny(roots, recheck) {
		return nil, errPathEscaped
	}

	names, err := f.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	entries := make([]browseEntry, 0, len(names))
	for _, name := range names {
		full := filepath.Join(dir, name)
		target, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue // dangling symlink or vanished entry
		}
		if len(roots) > 0 && !withinAny(roots, target) {
			continue // do not expose escapes out of the configured roots
		}
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		entries = append(entries, browseEntry{Name: name, Path: full, IsDir: info.IsDir()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func withinAny(roots []string, target string) bool {
	for _, root := range roots {
		if within(root, target) {
			return true
		}
	}
	return false
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
