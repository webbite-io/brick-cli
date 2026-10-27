package main

import (
	"encoding/json"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

// handleGetExcludeDirs serves GET /config/exclude-dirs: the folders excluded
// from sync for the active account, relative to its sync folder.
func (a *agentServer) handleGetExcludeDirs(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_not_running", "no sync is running to read settings from")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"excludeDirs": nonNil(a.settings.excludeDirs())})
}

// handleSetExcludeDirs serves PUT /config/exclude-dirs with a body of
// {"excludeDirs": [...]}, replacing the whole list. The change is saved to
// config.yaml and applied to the running sync straight away. Local copies of
// newly excluded folders are left on disk, just no longer synced; folders no
// longer excluded are downloaded on the next sync pass.
func (a *agentServer) handleSetExcludeDirs(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_not_running", "no sync is running to apply settings to")
		return
	}
	var req struct {
		ExcludeDirs *[]string `json:"excludeDirs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ExcludeDirs == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"excludeDirs": [...]}`)
		return
	}
	dirs, err := normalizeExcludeDirs(*req.ExcludeDirs)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.settings.setExcludeDirs(dirs); err != nil {
		writeError(w, http.StatusInternalServerError, "config_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"excludeDirs": nonNil(dirs)})
}

// normalizeExcludeDirs validates excludeDirs entries and puts them in the
// form the sync engine matches against: slash-separated, relative to the
// sync folder, cleaned, with no leading or trailing slash. Duplicates are
// dropped, keeping the first occurrence's position.
func normalizeExcludeDirs(dirs []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		clean := path.Clean("/" + strings.Trim(filepath.ToSlash(strings.TrimSpace(d)), "/"))
		clean = strings.TrimPrefix(clean, "/")
		if clean == "" || clean == "." {
			return nil, newAPIError(http.StatusBadRequest, "invalid_exclude_dir", "excludeDirs entries must name a folder")
		}
		// path.Clean on a rooted path resolves ".." away silently; reject it
		// instead, since it can only mean a typo or an attempt to escape.
		for _, seg := range strings.Split(filepath.ToSlash(d), "/") {
			if seg == ".." {
				return nil, newAPIError(http.StatusBadRequest, "invalid_exclude_dir", "excludeDirs entries must not contain \"..\"")
			}
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	return out, nil
}

// nonNil returns s, or an empty slice for nil, so it encodes as [] not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
