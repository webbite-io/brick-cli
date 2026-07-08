package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/fsnotify/fsnotify"
)

// tmpSuffix marks in-progress download files so the local tree walk and the
// watcher ignore them.
const tmpSuffix = ".brick-tmp"

// --- Storage API types (subset of the OpenAPI schema) ---

type storageNode struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId"`
	Name      string    `json:"name"`
	NodeType  string    `json:"nodeType"` // file, folder, root
	SizeBytes int64     `json:"sizeBytes"`
	Etag      string    `json:"etag"`
	Path      string    `json:"path"`
	IsDeleted bool      `json:"isDeleted"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type storageNodeList struct {
	Data  []storageNode `json:"data"`
	Count int64         `json:"count"`
}

type storageUploadResult struct {
	Node storageNode `json:"node"`
}

// --- Storage API client ---

// storageClient talks to the Storage API. Requests go to baseURL; token refresh
// goes to apiURL (the companion auth API). accountId is the tenant.
type storageClient struct {
	baseURL   string
	apiURL    string
	accountID string
	cfg       *Config
}

// request performs an authenticated request against /v1/accounts/{accountId}{path},
// transparently refreshing the access token once on a 401.
func (sc *storageClient) request(method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	full := "/v1/accounts/" + sc.accountID + path
	return authedRequest(sc.baseURL, sc.apiURL, method, full, body, headers, sc.cfg)
}

func (sc *storageClient) errFrom(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("storage API %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func (sc *storageClient) resolveRoot() (*storageNode, error) {
	resp, err := sc.request("GET", "/resolve", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, sc.errFrom(resp)
	}
	var node storageNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}
	return &node, nil
}

func (sc *storageClient) listChildren(parentID string) ([]storageNode, error) {
	var all []storageNode
	const limit = 200
	offset := 0
	for {
		path := fmt.Sprintf("/nodes/%s/children?limit=%d&offset=%d", parentID, limit, offset)
		resp, err := sc.request("GET", path, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := sc.errFrom(resp)
			resp.Body.Close()
			return nil, err
		}
		var list storageNodeList
		decErr := json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if decErr != nil {
			return nil, decErr
		}
		all = append(all, list.Data...)
		if len(list.Data) < limit {
			break
		}
		offset += limit
	}
	return all, nil
}

func (sc *storageClient) download(nodeID string) ([]byte, string, error) {
	resp, err := sc.request("GET", "/files/"+nodeID, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", sc.errFrom(resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return data, strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (sc *storageClient) upload(parentID, name string, data []byte) (*storageNode, error) {
	headers := map[string]string{
		"Content-Type": "application/octet-stream",
		"X-Parent-ID":  parentID,
		"X-Filename":   name,
	}
	resp, err := sc.request("POST", "/files", data, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, sc.errFrom(resp)
	}
	var result storageUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result.Node, nil
}

func (sc *storageClient) replace(nodeID string, data []byte) (*storageNode, error) {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	resp, err := sc.request("PUT", "/files/"+nodeID, data, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, sc.errFrom(resp)
	}
	var result storageUploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result.Node, nil
}

// delete soft-deletes a node (file or folder), moving it to trash. A 404 means
// it's already gone remotely, which is treated as success.
func (sc *storageClient) delete(nodeID string) error {
	resp, err := sc.request("DELETE", "/nodes/"+nodeID, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return sc.errFrom(resp)
	}
	return nil
}

func (sc *storageClient) createFolder(parentID, name string) (*storageNode, error) {
	body, _ := json.Marshal(map[string]string{"parentId": parentID, "name": name})
	resp, err := sc.request("POST", "/nodes", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// Folder already exists remotely — find and reuse it.
		children, err := sc.listChildren(parentID)
		if err != nil {
			return nil, err
		}
		for _, ch := range children {
			if ch.Name == name && ch.NodeType == "folder" && !ch.IsDeleted {
				n := ch
				return &n, nil
			}
		}
		return nil, fmt.Errorf("folder %q reported as conflict but not found", name)
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, sc.errFrom(resp)
	}
	var node storageNode
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return nil, err
	}
	return &node, nil
}

// folderSummary walks the entire remote tree under rootID and returns its
// direct folder children (for the onboarding "pick which folders to sync"
// prompt) along with the total size in bytes of every file in the tree.
func (sc *storageClient) folderSummary(rootID string) (topFolders []storageNode, totalSize int64, err error) {
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		children, err := sc.listChildren(id)
		if err != nil {
			return nil, 0, err
		}
		for _, ch := range children {
			if ch.IsDeleted {
				continue
			}
			switch ch.NodeType {
			case "folder":
				if id == rootID {
					topFolders = append(topFolders, ch)
				}
				queue = append(queue, ch.ID)
			case "file":
				totalSize += ch.SizeBytes
			}
		}
	}
	return topFolders, totalSize, nil
}

// tokenMu serializes token refreshes across the goroutines that share a single
// *Config. The sync engine's HTTP requests all refresh reactively on a
// 401/403. The auth server issues single-use refresh tokens with no grace
// period, so two goroutines spending the same refresh token concurrently
// leaves the loser rejected and can revoke the whole token chain, surfacing
// as repeated 401s until the process restarts.
var tokenMu sync.Mutex

// currentAccessToken returns cfg.AccessToken under the token lock. Callers
// snapshot the token they present this way and pass it back to
// rotateAccessToken, so a refresh races against a stable value rather than a
// field another goroutine may be rewriting.
func currentAccessToken(cfg *Config) string {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	return cfg.AccessToken
}

// rotateAccessToken refreshes cfg's tokens and returns the access token to
// retry with. presentedAccessToken is the token whose request was just
// rejected; if cfg.AccessToken no longer matches it, another goroutine already
// refreshed while we waited for the lock, so we adopt that result instead of
// replaying the now-revoked refresh token. The whole operation is serialized
// by tokenMu.
func rotateAccessToken(refreshAPIURL string, cfg *Config, presentedAccessToken string) (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()

	// A concurrent goroutine already rotated the token we presented; adopt it.
	if cfg.AccessToken != "" && cfg.AccessToken != presentedAccessToken {
		return cfg.AccessToken, nil
	}
	if cfg.RefreshToken == "" {
		return "", errors.New("no refresh token available")
	}

	clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
	newAccess, newRefresh, newID, err := refreshAccessToken(refreshAPIURL, cfg.RefreshToken, clientID)
	if err != nil {
		return "", err
	}
	if newAccess != "" {
		cfg.AccessToken = newAccess
	}
	if newRefresh != "" {
		cfg.RefreshToken = newRefresh
	}
	if newID != "" {
		cfg.IDToken = newID
	}
	if err := saveConfig(cfg); err != nil {
		return "", err
	}
	return cfg.AccessToken, nil
}

// authedRequest performs an authenticated request with a binary body and custom
// headers, refreshing tokens once on a 401. reqBaseURL is the target service;
// refreshAPIURL is the companion auth API used for token refresh.
//
// The Storage API resolves the caller via the companion API's storage-authz
// endpoint, which recognizes first-party session JWTs and delegated OAuth2
// access tokens (introspected for their brick/brick:manage scopes) — not the
// OIDC ID token, which that endpoint rejects. So this helper sends
// cfg.AccessToken as the bearer, same as the rest of the CLI.
func authedRequest(reqBaseURL, refreshAPIURL, method, path string, body []byte, headers map[string]string, cfg *Config) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	doRequest := func(token string) (*http.Response, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, strings.TrimRight(reqBaseURL, "/")+path, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return client.Do(req)
	}

	presented := currentAccessToken(cfg)
	resp, err := doRequest(presented)
	if err != nil {
		return nil, err
	}
	// The Storage API may report an expired token as 403 (it proxies token
	// validation to the auth API and historically collapsed a 401 there into a
	// 403), so refresh-and-retry on both statuses rather than 401 alone.
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && cfg.RefreshToken != "" {
		resp.Body.Close()
		newAccess, refreshErr := rotateAccessToken(refreshAPIURL, cfg, presented)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; token refresh failed: %v", errSessionExpired, refreshErr)
		}
		if newAccess == "" {
			return nil, fmt.Errorf("%w; no access token available (run 'brick --login')", errSessionExpired)
		}
		return doRequest(newAccess)
	}
	return resp, nil
}

// --- Sync state index (reconciliation source of truth) ---

// SyncEntry records the last-synced state of a single file, keyed by its path
// relative to the sync folder. It is what breaks the download/upload echo loop:
// a freshly downloaded file hashes to LocalHash, and a freshly uploaded file
// carries RemoteEtag, so neither re-triggers the opposite direction.
type SyncEntry struct {
	RelPath    string    `json:"relPath"`
	NodeID     string    `json:"nodeId"`
	RemoteEtag string    `json:"remoteEtag"`
	LocalHash  string    `json:"localHash"`
	LocalSize  int64     `json:"localSize"`
	SyncedAt   time.Time `json:"syncedAt"`
}

type SyncState struct {
	Folder  string               `json:"folder"`
	Entries map[string]SyncEntry `json:"entries"`
	// Folders is the set of folder paths (relative, slash-separated) that have
	// been synced with the server. It is to directories what Entries is to
	// files: it lets reconcile tell a brand-new local folder (push) apart from
	// one that was synced and later deleted on the server (remove locally).
	Folders map[string]bool `json:"folders"`
}

// syncStatePath returns the state file for one account, keyed by its account
// ID (a UUID) rather than the sync folder, so each account keeps its own
// synced-state bookkeeping even if two accounts share the same local folder.
func syncStatePath(accountID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("sync-state-%s.json", accountID)
	return filepath.Join(home, ".config", "brick", name), nil
}

func loadSyncState(accountID, folder string) *SyncState {
	st := &SyncState{Folder: folder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}}
	path, err := syncStatePath(accountID)
	if err != nil {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var loaded SyncState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return st
	}
	if loaded.Entries == nil {
		loaded.Entries = map[string]SyncEntry{}
	}
	if loaded.Folders == nil {
		loaded.Folders = map[string]bool{}
	}
	loaded.Folder = folder
	return &loaded
}

// --- Sync engine ---

type syncEngine struct {
	sc        *storageClient
	folder    string
	accountID string
	rootID    string

	// excludeDirs are the configured excludeDirs entries (slash-separated,
	// relative to folder); files under them are never uploaded or downloaded.
	excludeDirs []string

	// firstSync is true only for the very first reconcileAll call of this
	// process. conflictMode ("device", "brick" or "copy") says how to resolve a
	// file that already exists on both sides with no prior sync history —
	// which can only happen during that first pass, when the sync folder was
	// pre-populated before ever being synced. Once firstSync is cleared, the
	// ordinary last-writer-wins-on-remote logic in reconcileFile applies to any
	// future coincidental double-create.
	firstSync    bool
	conflictMode string

	mu    sync.Mutex // serializes reconcile + state access
	state *SyncState

	downloaded atomic.Int64
	uploaded   atomic.Int64
	deleted    atomic.Int64

	recentMu        sync.Mutex
	recentlyWritten map[string]time.Time

	// paused gates reconcileAll calls from the debounce worker and poll
	// ticker (sync.go's runStorageSync goroutines) without tearing down the
	// fsnotify watcher, so resuming is instant rather than needing a full
	// rescan. Controlled via the local control API's /v1/pause and /v1/resume.
	paused atomic.Bool

	// ctrlMu guards the live-status fields below, which are read by the
	// control API's /v1/status handler from a separate goroutine. It is
	// deliberately distinct from mu (which serializes reconcile passes) so a
	// status read never blocks on an in-progress reconcile.
	ctrlMu        sync.RWMutex
	ctrlState     string // "starting" | "syncing" | "idle" | "error"
	ctrlLastError string
	ctrlLastSync  time.Time
	ctrlInFlight  *controlInFlight
	ctrlActivity  []controlActivityEvent
}

// controlInFlight describes the single file transfer in progress, if any.
type controlInFlight struct {
	RelPath   string `json:"relPath"`
	Direction string `json:"direction"` // "upload" | "download"
}

// controlActivityEvent is one entry in the bounded recent-activity feed the
// control API serves at /v1/activity.
type controlActivityEvent struct {
	Kind    string    `json:"kind"` // "upload" | "download" | "update" | "trash" | "remove" | "keep-both"
	RelPath string    `json:"relPath"`
	At      time.Time `json:"at"`
}

// controlActivityCap bounds the in-memory activity ring buffer.
const controlActivityCap = 200

// setState updates the coarse sync state reported by /v1/status. It does not
// touch paused: the control API overlays "paused" on top of whatever state
// is recorded here for as long as e.paused is set.
func (e *syncEngine) setState(s string) {
	e.ctrlMu.Lock()
	e.ctrlState = s
	e.ctrlMu.Unlock()
}

// setSynced records a successful reconcile pass.
func (e *syncEngine) setSynced() {
	e.ctrlMu.Lock()
	e.ctrlState = "idle"
	e.ctrlLastError = ""
	e.ctrlLastSync = time.Now()
	e.ctrlMu.Unlock()
}

// setSyncError records a failed reconcile pass.
func (e *syncEngine) setSyncError(err error) {
	e.ctrlMu.Lock()
	e.ctrlState = "error"
	e.ctrlLastError = err.Error()
	e.ctrlMu.Unlock()
}

// setInFlight/clearInFlight track the single file transfer in progress, if
// any, for /v1/status's inFlight field.
func (e *syncEngine) setInFlight(relPath, direction string) {
	e.ctrlMu.Lock()
	e.ctrlInFlight = &controlInFlight{RelPath: relPath, Direction: direction}
	e.ctrlMu.Unlock()
}

func (e *syncEngine) clearInFlight() {
	e.ctrlMu.Lock()
	e.ctrlInFlight = nil
	e.ctrlMu.Unlock()
}

// publishActivity appends to the bounded recent-activity feed, dropping the
// oldest entry once controlActivityCap is reached.
func (e *syncEngine) publishActivity(kind, relPath string) {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()
	e.ctrlActivity = append(e.ctrlActivity, controlActivityEvent{Kind: kind, RelPath: relPath, At: time.Now()})
	if len(e.ctrlActivity) > controlActivityCap {
		e.ctrlActivity = e.ctrlActivity[len(e.ctrlActivity)-controlActivityCap:]
	}
}

// recentActivity returns up to limit of the most recent activity events,
// newest first.
func (e *syncEngine) recentActivity(limit int) []controlActivityEvent {
	e.ctrlMu.RLock()
	defer e.ctrlMu.RUnlock()
	n := len(e.ctrlActivity)
	if limit > n {
		limit = n
	}
	out := make([]controlActivityEvent, limit)
	for i := 0; i < limit; i++ {
		out[i] = e.ctrlActivity[n-1-i]
	}
	return out
}

// setPaused toggles whether the debounce worker in runStorageSync is allowed
// to call reconcileAll.
func (e *syncEngine) setPaused(p bool) {
	e.paused.Store(p)
}

// controlStatus is the JSON shape served at /v1/status.
type controlStatus struct {
	State               string           `json:"state"`
	Folder              string           `json:"folder"`
	LastError           string           `json:"lastError,omitempty"`
	LastSyncCompletedAt time.Time        `json:"lastSyncCompletedAt,omitempty"`
	Counters            controlCounters  `json:"counters"`
	InFlight            *controlInFlight `json:"inFlight"`
}

type controlCounters struct {
	Uploaded   int64 `json:"uploaded"`
	Downloaded int64 `json:"downloaded"`
	Deleted    int64 `json:"deleted"`
}

// statusSnapshot builds the current /v1/status payload. paused overlays the
// underlying reconcile-derived state, matching setState's contract above.
func (e *syncEngine) statusSnapshot() controlStatus {
	e.ctrlMu.RLock()
	defer e.ctrlMu.RUnlock()
	state := e.ctrlState
	if e.paused.Load() {
		state = "paused"
	}
	return controlStatus{
		State:               state,
		Folder:              e.folder,
		LastError:           e.ctrlLastError,
		LastSyncCompletedAt: e.ctrlLastSync,
		Counters: controlCounters{
			Uploaded:   e.uploaded.Load(),
			Downloaded: e.downloaded.Load(),
			Deleted:    e.deleted.Load(),
		},
		InFlight: e.ctrlInFlight,
	}
}

func (e *syncEngine) markRecentlyWritten(abs string) {
	e.recentMu.Lock()
	e.recentlyWritten[abs] = time.Now()
	e.recentMu.Unlock()
}

func (e *syncEngine) isRecentlyWritten(abs string) bool {
	e.recentMu.Lock()
	defer e.recentMu.Unlock()
	t, ok := e.recentlyWritten[abs]
	if !ok {
		return false
	}
	if time.Since(t) > 3*time.Second {
		delete(e.recentlyWritten, abs)
		return false
	}
	return true
}

// buildRemoteTree walks the remote node tree and returns files and folders keyed
// by slash-separated path relative to the root, plus a folderID lookup ("" = root).
func (e *syncEngine) buildRemoteTree() (files, folders map[string]storageNode, folderID map[string]string, err error) {
	files = map[string]storageNode{}
	folders = map[string]storageNode{}
	folderID = map[string]string{"": e.rootID}

	type item struct{ id, rel string }
	queue := []item{{e.rootID, ""}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		children, listErr := e.sc.listChildren(cur.id)
		if listErr != nil {
			return nil, nil, nil, listErr
		}
		for _, ch := range children {
			if ch.IsDeleted {
				continue
			}
			rel := ch.Name
			if cur.rel != "" {
				rel = cur.rel + "/" + ch.Name
			}
			switch ch.NodeType {
			case "folder":
				folders[rel] = ch
				folderID[rel] = ch.ID
				queue = append(queue, item{ch.ID, rel})
			case "file":
				files[rel] = ch
			}
		}
	}
	return files, folders, folderID, nil
}

// buildLocalTree walks the sync folder and returns file paths (with sizes) and
// directory paths, keyed by slash-separated path relative to the folder.
func (e *syncEngine) buildLocalTree() (files map[string]int64, dirs map[string]bool, err error) {
	files = map[string]int64{}
	dirs = map[string]bool{}
	err = filepath.WalkDir(e.folder, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == e.folder {
			return nil
		}
		if strings.HasSuffix(path, tmpSuffix) {
			return nil
		}
		rel := filepath.ToSlash(mustRel(e.folder, path))
		if d.IsDir() {
			dirs[rel] = true
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		files[rel] = info.Size()
		return nil
	})
	return files, dirs, err
}

// reconcileAll performs a full two-way reconciliation between the remote tree and
// the local folder. It is idempotent and safe to call repeatedly; the sync index
// ensures already-synced files do no work. Deletions push from whichever side
// removed the file to the other (a local delete trashes the file remotely; a
// remote delete/trash removes it locally); on content conflicts (both sides
// changed the same file), remote wins.
func (e *syncEngine) reconcileAll() (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.setState("syncing")
	defer func() {
		// errSessionExpired ends the whole sync loop (see runStorageSync), so
		// surfacing it as a lingering "error" status would be misleading —
		// the process is shutting down, not stuck.
		if err != nil && !errors.Is(err, errSessionExpired) {
			e.setSyncError(err)
		} else if err == nil {
			e.setSynced()
		}
	}()

	remoteFiles, remoteFolders, folderID, err := e.buildRemoteTree()
	if err != nil {
		return err
	}
	localFiles, localDirs, err := e.buildLocalTree()
	if err != nil {
		return err
	}

	// 1. Detect previously-synced folders that are now missing locally -> the
	//    user deleted them -> push the removal to the server as a soft-delete
	//    (trash), the folder counterpart of deleteRemoteFile for files. The
	//    server cascades a folder delete to its whole subtree, so once a
	//    folder is trashed we prune that subtree from remoteFolders/
	//    remoteFiles/folderID (and the matching index entries) instead of
	//    letting later passes redundantly re-process each descendant.
	//    Shallowest-first, skipping any folder already covered by an ancestor's
	//    cascade in this same pass.
	var deletedDirs []string
	for rel := range e.state.Folders {
		if _, ok := remoteFolders[rel]; !ok {
			continue // already gone remotely; handled by the gone-folder pass below
		}
		if localDirs[rel] {
			continue // still present locally
		}
		deletedDirs = append(deletedDirs, rel)
	}
	sort.Slice(deletedDirs, func(i, j int) bool {
		return strings.Count(deletedDirs[i], "/") < strings.Count(deletedDirs[j], "/")
	})
	handledDeletes := map[string]bool{}
	for _, rel := range deletedDirs {
		if isUnderAny(rel, handledDeletes) {
			continue
		}
		if err := e.sc.delete(remoteFolders[rel].ID); err != nil {
			if errors.Is(err, errSessionExpired) {
				return err
			}
			log.Printf("delete folder %s: %v", rel, err)
			continue
		}
		handledDeletes[rel] = true
		e.pruneRemoteSubtree(rel, remoteFolders, remoteFiles, folderID)
		e.deleted.Add(1)
		log.Printf("🗑  trashed folder %s (deleted locally)", rel)
		e.publishActivity("trash-folder", rel)
	}

	// 2. Create missing local directories for remote folders, and record every
	//    remote folder in the index so later passes can recognise which local
	//    folders were once synced.
	for rel := range remoteFolders {
		if err := os.MkdirAll(filepath.Join(e.folder, filepath.FromSlash(rel)), 0o755); err != nil {
			log.Printf("mkdir %s: %v", rel, err)
		}
		e.state.Folders[rel] = true
	}

	// 3. Reconcile every file across the union of remote, local and index keys.
	//    File uploads create any missing remote parent folders on demand (see
	//    ensureRemoteFolder), so this runs before the folder push/delete passes.
	keys := map[string]struct{}{}
	for k := range remoteFiles {
		keys[k] = struct{}{}
	}
	for k := range localFiles {
		keys[k] = struct{}{}
	}
	for k := range e.state.Entries {
		keys[k] = struct{}{}
	}
	for rel := range keys {
		if err := e.reconcileFile(rel, remoteFiles, localFiles, remoteFolders, folderID); err != nil {
			if errors.Is(err, errSessionExpired) {
				return err
			}
			log.Printf("sync %s: %v", rel, err)
		}
	}

	// 4. Push genuinely new local folders (not on the server and never synced).
	//    This is what carries up empty directories the user just created; folders
	//    that hold new files were already created during the file pass.
	for rel := range localDirs {
		if _, ok := remoteFolders[rel]; ok {
			continue
		}
		if e.state.Folders[rel] {
			continue // was synced before -> server deletion, handled in pass 5
		}
		if _, err := e.ensureRemoteFolder(rel, remoteFolders, folderID); err != nil {
			if errors.Is(err, errSessionExpired) {
				return err
			}
			log.Printf("create folder %s: %v", rel, err)
		}
	}

	// 5. Remove local folders that were synced before but are now gone from the
	//    server (deepest first, so children empty out before their parents). We
	//    only remove empty directories: a folder still holding new local content
	//    is dropped from the index instead, so the next pass re-pushes it rather
	//    than deleting unsynced work.
	var goneDirs []string
	for rel := range e.state.Folders {
		if _, ok := remoteFolders[rel]; !ok {
			goneDirs = append(goneDirs, rel)
		}
	}
	sort.Slice(goneDirs, func(i, j int) bool {
		return strings.Count(goneDirs[i], "/") > strings.Count(goneDirs[j], "/")
	})
	for _, rel := range goneDirs {
		delete(e.state.Folders, rel)
		abs := filepath.Join(e.folder, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil {
			continue // already gone locally
		}
		if !info.IsDir() {
			continue
		}
		if err := os.Remove(abs); err != nil {
			if !os.IsNotExist(err) {
				// Non-empty (holds new local content) -> leave it; having dropped
				// it from the index, pass 4 will re-push it on the next run.
				log.Printf("keeping %s: %v", rel, err)
			}
			continue
		}
		e.deleted.Add(1)
		log.Printf("🗑  removed folder %s (deleted on server)", rel)
		e.publishActivity("remove-folder", rel)
	}

	e.saveStateLocked()
	return nil
}

// pruneRemoteSubtree removes rel (a folder) and everything nested under it from
// the in-memory remote tree and the sync index, after the server has soft-
// deleted that folder (which cascades to its descendants). This keeps the rest
// of reconcileAll from redundantly re-processing files and folders that are
// already gone.
func (e *syncEngine) pruneRemoteSubtree(rel string, remoteFolders, remoteFiles map[string]storageNode, folderID map[string]string) {
	prefix := rel + "/"
	delete(remoteFolders, rel)
	delete(folderID, rel)
	delete(e.state.Folders, rel)
	for k := range remoteFolders {
		if strings.HasPrefix(k, prefix) {
			delete(remoteFolders, k)
			delete(folderID, k)
			delete(e.state.Folders, k)
		}
	}
	for k := range remoteFiles {
		if strings.HasPrefix(k, prefix) {
			delete(remoteFiles, k)
			delete(e.state.Entries, k)
		}
	}
}

// isUnderAny reports whether rel is nested inside any path in handled.
func isUnderAny(rel string, handled map[string]bool) bool {
	for h := range handled {
		if strings.HasPrefix(rel, h+"/") {
			return true
		}
	}
	return false
}

// isExcludedPath reports whether rel (a slash-separated path relative to the
// sync folder) is one of excludeDirs or nested below one of them.
func isExcludedPath(rel string, excludeDirs []string) bool {
	for _, dir := range excludeDirs {
		dir = strings.Trim(filepath.ToSlash(dir), "/")
		if dir == "" {
			continue
		}
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// reconcileFile applies the per-path reconciliation rules: creates, updates and
// deletes push from whichever side changed to the other; on a genuine content
// conflict (both sides changed the same file), remote wins.
func (e *syncEngine) reconcileFile(rel string, remoteFiles map[string]storageNode, localFiles map[string]int64, remoteFolders map[string]storageNode, folderID map[string]string) error {
	if isExcludedPath(rel, e.excludeDirs) {
		return e.reconcileExcludedFile(rel, remoteFiles, localFiles)
	}

	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	remoteNode, hasRemote := remoteFiles[rel]
	_, localExists := localFiles[rel]
	entry, hasEntry := e.state.Entries[rel]

	switch {
	case hasRemote && !localExists:
		if hasEntry {
			// Was synced before and is now gone locally -> the user deleted it ->
			// push that removal to the server as a soft-delete (trash).
			return e.deleteRemoteFile(rel, remoteNode.ID)
		}
		// Remote-only, never synced locally -> download.
		return e.downloadFile(rel, remoteNode)

	case !hasRemote && localExists:
		localHash, err := hashFile(abs)
		if err != nil {
			return err
		}
		if hasEntry && entry.LocalHash == localHash {
			// Deleted on server, unchanged locally -> remote wins, remove local.
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			delete(e.state.Entries, rel)
			e.deleted.Add(1)
			log.Printf("🗑  removed %s (deleted on server)", rel)
			e.publishActivity("remove", rel)
			return nil
		}
		// Local-only new file (or locally changed after a server delete) -> upload.
		return e.uploadNewFile(rel, remoteFolders, folderID)

	case hasRemote && localExists:
		localHash, err := hashFile(abs)
		if err != nil {
			return err
		}
		remoteChanged := !hasEntry || entry.RemoteEtag != remoteNode.Etag
		localChanged := !hasEntry || entry.LocalHash != localHash
		switch {
		case !remoteChanged && !localChanged:
			return nil // in sync
		case e.firstSync && !hasEntry:
			// Present on both sides with no prior sync history: this is the
			// pre-existing-folder conflict the onboarding wizard asked about.
			return e.applyFirstSyncConflict(rel, remoteNode, remoteFolders, folderID)
		case localChanged && !remoteChanged:
			return e.replaceFile(rel, remoteNode.ID)
		default:
			// remote changed (with or without a local change) -> remote wins.
			return e.downloadFile(rel, remoteNode)
		}

	default:
		// Gone on both sides — drop the stale index entry.
		if hasEntry {
			delete(e.state.Entries, rel)
		}
		return nil
	}
}

// reconcileExcludedFile handles a path under a configured excludeDirs entry:
// it is never uploaded or downloaded. The index entry is kept purely to
// detect when the file changes on either side, so each change is logged once
// rather than on every reconcile pass.
func (e *syncEngine) reconcileExcludedFile(rel string, remoteFiles map[string]storageNode, localFiles map[string]int64) error {
	remoteNode, hasRemote := remoteFiles[rel]
	_, localExists := localFiles[rel]
	entry, hasEntry := e.state.Entries[rel]

	if !hasRemote && !localExists {
		if hasEntry {
			delete(e.state.Entries, rel)
		}
		return nil
	}

	var localHash string
	if localExists {
		abs := filepath.Join(e.folder, filepath.FromSlash(rel))
		h, err := hashFile(abs)
		if err != nil {
			return err
		}
		localHash = h
	}

	if hasEntry && entry.LocalHash == localHash && entry.RemoteEtag == remoteNode.Etag {
		return nil // already logged, nothing changed since
	}

	switch {
	case localExists && !hasRemote:
		log.Printf("⊘ ignored local change to %s (excluded directory)", rel)
	case hasRemote && !localExists:
		log.Printf("⊘ ignored remote change to %s (excluded directory)", rel)
	default:
		log.Printf("⊘ ignored change to %s (excluded directory)", rel)
	}

	e.state.Entries[rel] = SyncEntry{
		RelPath:    rel,
		NodeID:     remoteNode.ID,
		RemoteEtag: remoteNode.Etag,
		LocalHash:  localHash,
		SyncedAt:   time.Now(),
	}
	return nil
}

// applyFirstSyncConflict resolves a file present on both sides with no sync
// history, per the mode chosen in the onboarding wizard (e.conflictMode):
// "device" downloads the remote copy over the local one, "brick" uploads the
// local copy over the remote one, and "copy" keeps both by renaming the local
// file aside before downloading the remote one to the original path.
func (e *syncEngine) applyFirstSyncConflict(rel string, remoteNode storageNode, remoteFolders map[string]storageNode, folderID map[string]string) error {
	switch e.conflictMode {
	case "brick":
		return e.replaceFile(rel, remoteNode.ID)
	case "copy":
		return e.keepBothFile(rel, remoteNode, remoteFolders, folderID)
	default: // "device", or unset (folder was empty, so this case is unreachable in practice)
		return e.downloadFile(rel, remoteNode)
	}
}

// keepBothFile resolves a first-sync conflict by keeping both copies: the
// local file is renamed aside and re-uploaded as a new file, and the remote
// copy is downloaded to the original path.
func (e *syncEngine) keepBothFile(rel string, remoteNode storageNode, remoteFolders map[string]storageNode, folderID map[string]string) error {
	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	copyRel := dupPath(rel)
	copyAbs := filepath.Join(e.folder, filepath.FromSlash(copyRel))
	if err := os.Rename(abs, copyAbs); err != nil {
		return err
	}
	if err := e.downloadFile(rel, remoteNode); err != nil {
		return err
	}
	if err := e.uploadNewFile(copyRel, remoteFolders, folderID); err != nil {
		return err
	}
	log.Printf("⧉ kept both copies of %s (as %s)", rel, copyRel)
	e.publishActivity("keep-both", rel)
	return nil
}

// dupPath returns rel with " (copy)" inserted before the file extension,
// used to keep a local file's pre-sync content under a new name.
func dupPath(rel string) string {
	dir := parentOf(rel)
	base := baseName(rel)
	ext := filepath.Ext(base)
	newBase := strings.TrimSuffix(base, ext) + " (copy)" + ext
	if dir == "" {
		return newBase
	}
	return dir + "/" + newBase
}

func (e *syncEngine) downloadFile(rel string, node storageNode) error {
	e.setInFlight(rel, "download")
	defer e.clearInFlight()

	data, etag, err := e.sc.download(node.ID)
	if err != nil {
		return err
	}
	if etag == "" {
		etag = node.Etag
	}
	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	e.markRecentlyWritten(abs)
	if err := atomicWrite(abs, data); err != nil {
		return err
	}
	e.state.Entries[rel] = SyncEntry{
		RelPath:    rel,
		NodeID:     node.ID,
		RemoteEtag: etag,
		LocalHash:  hashBytes(data),
		LocalSize:  int64(len(data)),
		SyncedAt:   time.Now(),
	}
	e.downloaded.Add(1)
	log.Printf("↓ downloaded %s", rel)
	e.publishActivity("download", rel)
	return nil
}

// deleteRemoteFile pushes a local file removal to the server as a soft-delete
// (trash), the mirror image of downloadFile pushing a remote removal to local.
func (e *syncEngine) deleteRemoteFile(rel, nodeID string) error {
	if err := e.sc.delete(nodeID); err != nil {
		return err
	}
	delete(e.state.Entries, rel)
	e.deleted.Add(1)
	log.Printf("🗑  trashed %s (deleted locally)", rel)
	e.publishActivity("trash", rel)
	return nil
}

// ensureRemoteFolder returns the server node ID for the folder at rel, creating
// it (and any missing ancestors) on demand. It records every folder it touches
// in folderID, remoteFolders and the sync index so the rest of reconcile sees a
// consistent tree. rel == "" resolves to the sync root.
func (e *syncEngine) ensureRemoteFolder(rel string, remoteFolders map[string]storageNode, folderID map[string]string) (string, error) {
	if rel == "" {
		return e.rootID, nil
	}
	if id, ok := folderID[rel]; ok {
		return id, nil
	}
	parentID, err := e.ensureRemoteFolder(parentOf(rel), remoteFolders, folderID)
	if err != nil {
		return "", err
	}
	node, err := e.sc.createFolder(parentID, baseName(rel))
	if err != nil {
		return "", err
	}
	folderID[rel] = node.ID
	remoteFolders[rel] = *node
	e.state.Folders[rel] = true
	return node.ID, nil
}

func (e *syncEngine) uploadNewFile(rel string, remoteFolders map[string]storageNode, folderID map[string]string) error {
	e.setInFlight(rel, "upload")
	defer e.clearInFlight()

	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	parentID, err := e.ensureRemoteFolder(parentOf(rel), remoteFolders, folderID)
	if err != nil {
		return err
	}
	node, err := e.sc.upload(parentID, baseName(rel), data)
	if err != nil {
		return err
	}
	e.recordUpload(rel, node, data)
	log.Printf("↑ uploaded %s", rel)
	e.publishActivity("upload", rel)
	return nil
}

func (e *syncEngine) replaceFile(rel, nodeID string) error {
	e.setInFlight(rel, "upload")
	defer e.clearInFlight()

	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	node, err := e.sc.replace(nodeID, data)
	if err != nil {
		return err
	}
	e.recordUpload(rel, node, data)
	log.Printf("↑ uploaded %s (updated)", rel)
	e.publishActivity("update", rel)
	return nil
}

func (e *syncEngine) recordUpload(rel string, node *storageNode, data []byte) {
	e.state.Entries[rel] = SyncEntry{
		RelPath:    rel,
		NodeID:     node.ID,
		RemoteEtag: node.Etag,
		LocalHash:  hashBytes(data),
		LocalSize:  int64(len(data)),
		SyncedAt:   time.Now(),
	}
	e.uploaded.Add(1)
}

func (e *syncEngine) saveState() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saveStateLocked()
}

func (e *syncEngine) saveStateLocked() {
	path, err := syncStatePath(e.accountID)
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

func (e *syncEngine) addWatchesRecursive(w *fsnotify.Watcher) {
	_ = filepath.WalkDir(e.folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = w.Add(path)
		}
		return nil
	})
}

func (e *syncEngine) printSummary() {
	fmt.Printf("\n--- Sync summary ---\n")
	fmt.Printf("Downloaded: %d\n", e.downloaded.Load())
	fmt.Printf("Uploaded:   %d\n", e.uploaded.Load())
	fmt.Printf("Removed:    %d\n", e.deleted.Load())
}

// syncSetup captures the outcome of the interactive part of starting a sync:
// authentication, sync folder resolution, and (on a brand new setup) the
// choice of how to resolve pre-existing local/remote conflicts and which
// folders to sync. runAsDaemon computes it once in the foreground, before
// detaching, and hands it to the detached child so the child's first
// reconcile pass applies those decisions rather than prompting a second time.
type syncSetup struct {
	cfg          *Config
	sc           *storageClient
	folder       string
	rootID       string
	conflictMode string
	isFirstSetup bool
}

// prepareSync runs the interactive setup steps needed before a sync can
// start: authentication, sync folder resolution (prompting on first run), a
// Storage API reachability check, and (on a brand new setup) the sync-scope
// onboarding.
func prepareSync(apiURL, storageURL string) (*syncSetup, error) {
	cfg, err := ensureAuthenticated(apiURL)
	if err != nil {
		return nil, err
	}
	if cfg.ActiveAccountID == "" {
		return nil, errors.New("no active account selected; run 'brick --switch-accounts' first")
	}

	ac := cfg.activeAccount()
	isFirstSetup := ac == nil || strings.TrimSpace(ac.StorageSyncFolder) == ""
	folder, conflictMode, err := ensureStorageSyncFolder(cfg)
	if err != nil {
		return nil, err
	}

	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}
	root, err := sc.resolveRoot()
	if err != nil {
		return nil, fmt.Errorf("could not reach storage API at %s: %w", storageURL, err)
	}

	if isFirstSetup {
		if err := runSyncScopeOnboarding(sc, root.ID, cfg); err != nil {
			return nil, err
		}
		if err := promptForRemoteControl(cfg); err != nil {
			return nil, err
		}
		fmt.Println("\nNow, we're ready. Brick will now sync your files...")
	}

	return &syncSetup{
		cfg:          cfg,
		sc:           sc,
		folder:       folder,
		rootID:       root.ID,
		conflictMode: conflictMode,
		isFirstSetup: isFirstSetup,
	}, nil
}

// runStorageSync performs an initial full sync of storageSyncFolder with the
// Storage API, then watches the folder and polls the API for changes until
// interrupted. Creates, updates and deletes all push from whichever side made
// the change to the other; simultaneous edits of the same file are the one
// case where the API's copy wins.
func runStorageSync(apiURL, storageURL string, remoteControl, noControlAPI bool) error {
	// Only one sync engine may run per user at a time — a second reconcileAll
	// loop against the same folder would race the first. Acquired before
	// anything else so a second invocation (e.g. a tray app trying to launch
	// brick when it's already running) fails fast instead of corrupting
	// state. Released automatically by the OS if this process dies, so a
	// crash never leaves a stale lock behind.
	lockPath, err := instanceLockPath()
	if err != nil {
		return err
	}
	lock, err := acquireInstanceLock(lockPath)
	if err != nil {
		if errors.Is(err, errInstanceLocked) {
			return errors.New("brick is already running for this user")
		}
		return err
	}
	defer lock.Release()

	setup, err := prepareSync(apiURL, storageURL)
	if err != nil {
		return err
	}
	return runSyncLoop(setup, remoteControl, noControlAPI, false)
}

// runSyncLoop builds the sync engine from setup and runs it — the initial
// full reconciliation, then the filesystem watcher and periodic poll — until
// interrupted (Ctrl+C/SIGTERM in the foreground case; SIGTERM when stopping a
// detached daemon). background is true when this is the detached daemon
// child (as opposed to a foreground run), and is recorded in the control
// discovery file so a later 'brick --switch-accounts' knows whether it's
// safe to relaunch a replacement daemon after stopping this one.
func runSyncLoop(setup *syncSetup, remoteControl, noControlAPI, background bool) error {
	cfg, sc, folder := setup.cfg, setup.sc, setup.folder
	// cfg.RemoteControl, set during onboarding (see promptForRemoteControl),
	// makes remote control the default without needing -r on every run; -r
	// still forces it on for this invocation even if the config default is off.
	remoteControl = remoteControl || cfg.RemoteControl
	ac := cfg.ensureActiveAccount()

	eng := &syncEngine{
		sc:              sc,
		folder:          folder,
		accountID:       cfg.ActiveAccountID,
		rootID:          setup.rootID,
		excludeDirs:     ac.ExcludeDirs,
		firstSync:       setup.isFirstSetup,
		conflictMode:    setup.conflictMode,
		state:           loadSyncState(cfg.ActiveAccountID, folder),
		recentlyWritten: map[string]time.Time{},
	}
	eng.setState("starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		cancel()
	}()

	fmt.Printf("Syncing %s with the Storage API. Press Ctrl+C to stop.\n", folder)

	// Remote file agent: register this device with the storage API. When
	// remoteControl is enabled, the allowed roots are also exposed so the same
	// user can browse/list/transfer files remotely; otherwise the storage API is
	// refused if it tries to call that functionality. Runs alongside sync;
	// failures here are non-fatal to syncing.
	agentRoots := resolveAgentRoots(cfg)
	agentSecret := newAgentSecret()
	if _, agentLn, aerr := startAgentServer(agentRoots, agentSecret, sc, remoteControl); aerr != nil {
		log.Printf("could not start remote file agent: %v", aerr)
	} else {
		agentAddr := agentLn.Addr().String()
		defer agentLn.Close()
		go connectAgentWithReconnect(ctx, sc.baseURL, sc.apiURL, cfg, agentSecret, agentAddr, remoteControl)
		defer deregisterAgent(sc.baseURL, sc.apiURL, cfg)
		if remoteControl {
			fmt.Printf("Remote control enabled (roots: %s)\n", strings.Join(agentRoots, ", "))
		}
	}

	// Initial full reconciliation (pulls everything, pushes local-only files).
	if err := eng.reconcileAll(); err != nil {
		if errors.Is(err, errSessionExpired) {
			return err
		}
		log.Printf("initial sync error: %v", err)
	}
	eng.firstSync = false

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Typically EMFILE when the system's inotify instance limit
		// (fs.inotify.max_user_instances) is exhausted; keep the surfaced
		// message vague since it lands in the user's console.
		return errors.New("could not create a filesystem watcher")
	}
	defer watcher.Close()
	eng.addWatchesRecursive(watcher)

	// Coalesce reconcile requests from the watcher and the poll ticker.
	trigger := make(chan struct{}, 1)
	notify := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	// Local status/control API: lets a local client (e.g. a tray app) read
	// live sync status and issue pause/resume/quit without touching the
	// terminal this process is attached to. Loopback-only (unix domain
	// socket), token-gated; see controlapi.go for the full protocol.
	if !noControlAPI {
		if cs, csErr := startControlServer(eng, cancel, notify, background, remoteControl, agentRootsFlag); csErr != nil {
			log.Printf("could not start control API: %v", csErr)
		} else {
			defer cs.Close()
		}
	}

	// Debounced reconcile worker.
	go func() {
		var timer *time.Timer
		var timerC <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				if timer == nil {
					timer = time.NewTimer(750 * time.Millisecond)
					timerC = timer.C
				} else {
					timer.Reset(750 * time.Millisecond)
				}
			case <-timerC:
				timer, timerC = nil, nil
				if eng.paused.Load() {
					// Gate only the reconcile call itself (not the watcher or
					// ticker) so resuming is instant: notify() below already
					// woke this worker, so resume just needs to flip the flag.
					continue
				}
				if err := eng.reconcileAll(); err != nil {
					if errors.Is(err, errSessionExpired) {
						log.Printf("session expired — run 'brick --login' to re-authenticate")
						cancel()
						return
					}
					log.Printf("sync error: %v", err)
				}
				eng.addWatchesRecursive(watcher)
			}
		}
	}()

	// Periodic remote poll.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				notify()
			}
		}
	}()

	// Watcher event loop.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if strings.HasSuffix(ev.Name, tmpSuffix) {
					continue
				}
				if eng.isRecentlyWritten(ev.Name) {
					continue
				}
				notify()
			case werr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("watcher error: %v", werr)
			}
		}
	}()

	<-ctx.Done()
	eng.saveState()
	eng.printSummary()
	return nil
}

// ensureStorageSyncFolder returns the configured sync folder, prompting the user
// to set it if unset, expanding ~, creating it on disk, and persisting it.
// conflictMode is only meaningful the first time a folder is configured, and is
// empty otherwise; it reports how pre-existing local files that already have a
// remote counterpart should be reconciled during the very first sync.
func ensureStorageSyncFolder(cfg *Config) (folder string, conflictMode string, err error) {
	ac := cfg.ensureActiveAccount()
	folder = strings.TrimSpace(ac.StorageSyncFolder)
	if folder == "" {
		folder, conflictMode, err = promptForSyncFolder()
		if err != nil {
			return "", "", err
		}
	}

	if strings.HasPrefix(folder, "~") {
		if home, herr := os.UserHomeDir(); herr == nil {
			folder = filepath.Join(home, strings.TrimPrefix(folder, "~"))
		}
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", "", fmt.Errorf("could not create sync folder: %w", err)
	}
	if ac.StorageSyncFolder != abs {
		ac.StorageSyncFolder = abs
		if err := saveConfig(cfg); err != nil {
			return "", "", err
		}
		fmt.Printf("Saved storageSyncFolder = %s\n", abs)
	}
	return abs, conflictMode, nil
}

// runSyncScopeOnboarding runs the final onboarding step: if the Brick account
// has any top-level folders, it asks whether to sync everything or only a
// subset, writing the folders the user picks to exclude into the active
// account's ExcludeDirs. A no-op if the account has no folders yet.
func runSyncScopeOnboarding(sc *storageClient, rootID string, cfg *Config) error {
	topFolders, totalSize, err := sc.folderSummary(rootID)
	if err != nil {
		return err
	}
	if len(topFolders) == 0 {
		return nil
	}

	fmt.Println("\nOne last decision to make:")
	fmt.Println()

	const (
		optAll  = "all"
		optPick = "pick"
	)
	var choice string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Sync scope").
			Options(
				huh.NewOption(fmt.Sprintf("Sync all folders from Brick (%s)", humanSize(totalSize)), optAll),
				huh.NewOption("Pick which folders to sync", optPick),
			).
			Value(&choice),
	)).Run(); err != nil {
		return err
	}
	if choice == optAll {
		return nil
	}

	options := make([]huh.Option[string], len(topFolders))
	for i, f := range topFolders {
		options[i] = huh.NewOption(f.Name, f.Name)
	}
	var excludeDirs []string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Select the folders to exclude from sync").
			Options(options...).
			Value(&excludeDirs),
	)).Run(); err != nil {
		return err
	}

	cfg.ensureActiveAccount().ExcludeDirs = excludeDirs
	return saveConfig(cfg)
}

// promptForSyncFolder interactively asks the user to pick a sync folder — the
// default ~/Brick or a custom folder browsed via a huh file picker — and, if
// that folder already contains files, how to resolve conflicts with the
// remote copy during the first sync.
func promptForSyncFolder() (folder string, conflictMode string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("could not determine home directory: %w", err)
	}
	defaultFolder := filepath.Join(home, "Brick")

	fmt.Println("\nYou have no sync folder configured (storageSyncFolder).")
	fmt.Println()

	const (
		optDefault = "default"
		optCustom  = "custom"
	)
	var choice string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Choose a sync folder").
			Options(
				huh.NewOption(fmt.Sprintf("Use %s", defaultFolder), optDefault),
				huh.NewOption("Custom folder", optCustom),
			).
			Value(&choice),
	)).Run(); err != nil {
		return "", "", err
	}

	if choice == optDefault {
		folder = defaultFolder
	} else {
		var picked string
		if err := huh.NewForm(huh.NewGroup(
			huh.NewFilePicker().
				Title("Pick a folder to sync").
				CurrentDirectory(home).
				DirAllowed(true).
				FileAllowed(false).
				Picking(true).
				Value(&picked),
		)).Run(); err != nil {
			return "", "", err
		}
		folder = picked
	}

	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", "", err
	}
	hasFiles, err := dirHasEntries(abs)
	if err != nil {
		return "", "", err
	}
	if hasFiles {
		conflictMode, err = promptConflictMode()
		if err != nil {
			return "", "", err
		}
	}
	return abs, conflictMode, nil
}

// promptForRemoteControl asks, during first-time onboarding, whether to
// enable remote file access (-r/--remote-control) by default and, if so,
// which folder to expose as its root. The choice is persisted to
// cfg.RemoteControl/cfg.AgentRoots so it takes effect on every future run
// without needing -r; declining here just means -r can still be used to
// force it on for a single invocation later.
func promptForRemoteControl(cfg *Config) error {
	enable := true
	if err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Do you want to remotely access files on this device via Brick?").
			Affirmative("Yes").
			Negative("No").
			Value(&enable),
	)).Run(); err != nil {
		return err
	}
	if !enable {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home directory: %w", err)
	}

	const (
		optHome   = "home"
		optCustom = "custom"
	)
	choice := optHome
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which folder should be accessible remotely?").
			Options(
				huh.NewOption(fmt.Sprintf("Home folder (%s)", home), optHome),
				huh.NewOption("Custom folder", optCustom),
			).
			Value(&choice),
	)).Run(); err != nil {
		return err
	}

	root := home
	if choice == optCustom {
		var picked string
		if err := huh.NewForm(huh.NewGroup(
			huh.NewFilePicker().
				Title("Pick a folder to expose remotely").
				CurrentDirectory("/").
				DirAllowed(true).
				FileAllowed(false).
				Picking(true).
				Value(&picked),
		)).Run(); err != nil {
			return err
		}
		root = picked
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	cfg.RemoteControl = true
	present := false
	for _, r := range cfg.AgentRoots {
		if r == abs {
			present = true
			break
		}
	}
	if !present {
		cfg.AgentRoots = append(cfg.AgentRoots, abs)
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	fmt.Println("\nRemote file access is now ON by default. Disable via remoteControl flag in config file.\n")
	return nil
}

// promptConflictMode asks how to resolve files that already exist both
// locally (in a pre-existing, non-empty sync folder) and remotely, before any
// sync history exists for them.
func promptConflictMode() (string, error) {
	fmt.Println("Your sync folder already contains files, how should we handle possible conflicts?")
	fmt.Println()
	var mode string
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Conflict resolution").
			Options(
				huh.NewOption("Overwrite the file on this device.", "device"),
				huh.NewOption("Overwrite the file on Brick.", "brick"),
				huh.NewOption("Make a copy (so nothing is lost).", "copy"),
			).
			Value(&mode),
	)).Run()
	return mode, err
}

// dirHasEntries reports whether path exists and contains at least one entry.
func dirHasEntries(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return len(entries) > 0, nil
}

// humanSize renders bytes as a human-readable size (e.g. "12.3 GB").
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// --- small helpers ---

func atomicWrite(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}

func parentOf(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

func baseName(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
