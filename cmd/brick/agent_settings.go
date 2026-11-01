package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

// settingsResponse is the body of GET and PATCH /settings. selectiveSync can
// be changed with PATCH; roots, with whether each is read-only, is listed only,
// as it can only be changed on the device itself.
type settingsResponse struct {
	SelectiveSync selectiveSyncSettings `json:"selectiveSync"`
	Roots         []rootEntry           `json:"roots"`
}

// selectiveSyncSettings holds the folders excluded from sync for the active
// account, relative to its sync folder.
type selectiveSyncSettings struct {
	ExcludeDirs []string `json:"excludeDirs"`
}

func (a *agentServer) settingsResponse() settingsResponse {
	return settingsResponse{
		SelectiveSync: selectiveSyncSettings{ExcludeDirs: nonNil(a.settings.excludeDirs())},
		Roots:         a.rootEntries(),
	}
}

// handleGetSettings serves GET /settings.
func (a *agentServer) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_not_running", "No sync is running to read settings from.")
		return
	}
	writeJSON(w, http.StatusOK, a.settingsResponse())
}

// handleUpdateSettings serves PATCH /settings with a body of
// {"selectiveSync": {"excludeDirs": [...]}}, replacing the whole exclude list,
// and answers with the settings as GET /settings does. The change is saved to
// config.yaml and applied to the running sync straight away. Local copies of
// newly excluded folders are left on disk, just no longer synced; folders no
// longer excluded are downloaded on the next sync pass. A body that names
// roots is refused: which folders remote control may reach, and whether it
// may change them, is up to the device, not to remote control.
func (a *agentServer) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if a.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_not_running", "No sync is running to apply settings to.")
		return
	}
	const usage = `The body must be {"selectiveSync": {"excludeDirs": [...]}}.`
	var req map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", usage)
		return
	}
	if _, ok := req["roots"]; ok {
		writeError(w, http.StatusForbidden, "setting_read_only", "Roots and whether they're read-only can only be changed on the device.")
		return
	}
	raw, ok := req["selectiveSync"]
	if !ok || len(req) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", usage)
		return
	}
	var sel struct {
		ExcludeDirs *[]string `json:"excludeDirs"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sel); err != nil || sel.ExcludeDirs == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", usage)
		return
	}
	dirs, err := normalizeExcludeDirs(*sel.ExcludeDirs)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.settings.setExcludeDirs(dirs); err != nil {
		writeError(w, http.StatusInternalServerError, "config_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.settingsResponse())
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
			return nil, newAPIError(http.StatusBadRequest, "invalid_exclude_dir", "Each excludeDirs entry must name a folder.")
		}
		// path.Clean on a rooted path resolves ".." away silently; reject it
		// instead, since it can only mean a typo or an attempt to escape.
		for _, seg := range strings.Split(filepath.ToSlash(d), "/") {
			if seg == ".." {
				return nil, newAPIError(http.StatusBadRequest, "invalid_exclude_dir", "No excludeDirs entry may contain \"..\".")
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
