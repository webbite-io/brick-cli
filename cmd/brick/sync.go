package main

import (
	"bufio"
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
	"time"

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

func syncStatePath(folder string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(folder))
	name := fmt.Sprintf("sync-state-%s.json", hex.EncodeToString(h[:8]))
	return filepath.Join(home, ".config", "brick", name), nil
}

func loadSyncState(folder string) *SyncState {
	st := &SyncState{Folder: folder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}}
	path, err := syncStatePath(folder)
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
	sc     *storageClient
	folder string
	rootID string

	mu    sync.Mutex // serializes reconcile + state access
	state *SyncState

	downloaded int
	uploaded   int
	deleted    int

	recentMu        sync.Mutex
	recentlyWritten map[string]time.Time
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
// ensures already-synced files do no work. Remote always wins on conflict.
func (e *syncEngine) reconcileAll() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	remoteFiles, remoteFolders, folderID, err := e.buildRemoteTree()
	if err != nil {
		return err
	}
	localFiles, localDirs, err := e.buildLocalTree()
	if err != nil {
		return err
	}

	// 1. Create missing local directories for remote folders, and record every
	//    remote folder in the index so later passes can recognise which local
	//    folders were once synced.
	for rel := range remoteFolders {
		if err := os.MkdirAll(filepath.Join(e.folder, filepath.FromSlash(rel)), 0o755); err != nil {
			log.Printf("mkdir %s: %v", rel, err)
		}
		e.state.Folders[rel] = true
	}

	// 2. Reconcile every file across the union of remote, local and index keys.
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

	// 3. Push genuinely new local folders (not on the server and never synced).
	//    This is what carries up empty directories the user just created; folders
	//    that hold new files were already created during the file pass.
	for rel := range localDirs {
		if _, ok := remoteFolders[rel]; ok {
			continue
		}
		if e.state.Folders[rel] {
			continue // was synced before -> server deletion, handled in pass 4
		}
		if _, err := e.ensureRemoteFolder(rel, remoteFolders, folderID); err != nil {
			if errors.Is(err, errSessionExpired) {
				return err
			}
			log.Printf("create folder %s: %v", rel, err)
		}
	}

	// 4. Remove local folders that were synced before but are now gone from the
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
				// it from the index, pass 3 will re-push it on the next run.
				log.Printf("keeping %s: %v", rel, err)
			}
			continue
		}
		e.deleted++
		log.Printf("🗑  removed folder %s (deleted on server)", rel)
	}

	e.saveStateLocked()
	return nil
}

// reconcileFile applies the per-path reconciliation rules (remote wins on conflict).
func (e *syncEngine) reconcileFile(rel string, remoteFiles map[string]storageNode, localFiles map[string]int64, remoteFolders map[string]storageNode, folderID map[string]string) error {
	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	remoteNode, hasRemote := remoteFiles[rel]
	_, localExists := localFiles[rel]
	entry, hasEntry := e.state.Entries[rel]

	switch {
	case hasRemote && !localExists:
		// Remote-only (new on server) or locally deleted -> download (never delete remote).
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
			e.deleted++
			log.Printf("🗑  removed %s (deleted on server)", rel)
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

func (e *syncEngine) downloadFile(rel string, node storageNode) error {
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
	e.downloaded++
	log.Printf("↓ downloaded %s", rel)
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
	return nil
}

func (e *syncEngine) replaceFile(rel, nodeID string) error {
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
	e.uploaded++
}

func (e *syncEngine) saveState() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saveStateLocked()
}

func (e *syncEngine) saveStateLocked() {
	path, err := syncStatePath(e.folder)
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
	fmt.Printf("Downloaded: %d\n", e.downloaded)
	fmt.Printf("Uploaded:   %d\n", e.uploaded)
	fmt.Printf("Removed:    %d\n", e.deleted)
}

// runStorageSync performs an initial full sync of storageSyncFolder with the
// Storage API, then watches the folder and polls the API for changes until
// interrupted. The API always has precedence on conflict.
func runStorageSync(apiURL, storageURL string) error {
	cfg, err := ensureAuthenticated(apiURL)
	if err != nil {
		return err
	}
	if cfg.AccountID == "" {
		return errors.New("no active account selected; run 'brick --switch-accounts' first")
	}

	folder, err := ensureStorageSyncFolder(cfg)
	if err != nil {
		return err
	}

	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.AccountID, cfg: cfg}
	root, err := sc.resolveRoot()
	if err != nil {
		return fmt.Errorf("could not reach storage API at %s: %w", storageURL, err)
	}

	eng := &syncEngine{
		sc:              sc,
		folder:          folder,
		rootID:          root.ID,
		state:           loadSyncState(folder),
		recentlyWritten: map[string]time.Time{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println()
		cancel()
	}()

	fmt.Printf("Syncing %s with the Storage API. Press Ctrl+C to stop.\n", folder)

	// Remote file agent: register this device with the storage API and expose the
	// allowed roots so the same user can browse/transfer files remotely. Runs
	// alongside sync; failures here are non-fatal to syncing.
	agentRoots := resolveAgentRoots(cfg, folder)
	agentSecret := newAgentSecret()
	if _, agentLn, aerr := startAgentServer(agentRoots, agentSecret, sc); aerr != nil {
		log.Printf("could not start remote file agent: %v", aerr)
	} else {
		agentAddr := agentLn.Addr().String()
		defer agentLn.Close()
		go connectAgentWithReconnect(ctx, storageURL, apiURL, cfg, agentSecret, agentAddr)
		defer deregisterAgent(storageURL, apiURL, cfg)
		fmt.Printf("Remote file agent enabled (roots: %s)\n", strings.Join(agentRoots, ", "))
	}

	// Initial full reconciliation (pulls everything, pushes local-only files).
	if err := eng.reconcileAll(); err != nil {
		if errors.Is(err, errSessionExpired) {
			return err
		}
		log.Printf("initial sync error: %v", err)
	}

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
func ensureStorageSyncFolder(cfg *Config) (string, error) {
	folder := strings.TrimSpace(cfg.StorageSyncFolder)
	if folder == "" {
		fmt.Println("No storage sync folder is configured (storageSyncFolder).")
		fmt.Print("Enter the local folder to sync with the Storage API: ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("could not read input: %w", err)
		}
		folder = strings.TrimSpace(line)
		if folder == "" {
			return "", errors.New("no folder provided")
		}
	}

	if strings.HasPrefix(folder, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			folder = filepath.Join(home, strings.TrimPrefix(folder, "~"))
		}
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("could not create sync folder: %w", err)
	}
	if cfg.StorageSyncFolder != abs {
		cfg.StorageSyncFolder = abs
		if err := saveConfig(cfg); err != nil {
			return "", err
		}
		fmt.Printf("Saved storageSyncFolder = %s\n", abs)
	}
	return abs, nil
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
