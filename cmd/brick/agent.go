package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// agentSecretHeader must match agent.SecretHeader on the storage side. This is a
// protocol-level constant shared with rbite (the storage API's remote-agent
// registry hardcodes this exact header name for any attaching client, brick
// included), so it is intentionally not rebranded. The storage API injects the
// per-session secret on every proxied request; the local agent listener only
// honors requests carrying it, so other local processes that stumble onto the
// loopback port cannot use it.
const agentSecretHeader = "X-RBite-Agent-Secret"

// yamuxConfig returns the yamux configuration used for the agent tunnel. It
// starts from yamux's defaults but raises ConnectionWriteTimeout: the default
// (10s) is the window yamux's own internal keepalive ping has to get a pong
// back before it kills the whole session with "keepalive timeout" — too tight
// for a public-internet WAN link, especially since we already run our own
// WebSocket-level ping (see wsConn.ping and the storage API's equivalent) to
// keep idle proxies alive. A brief latency spike shouldn't tear down the
// tunnel and force a full reconnect.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.ConnectionWriteTimeout = 30 * time.Second
	// yamux's default LogOutput is os.Stderr, which builds a private logger
	// that bypasses log.SetOutput — so its "[ERR] yamux: ..." lines would be
	// painted over the sync TUI instead of landing in its log window (and
	// would miss brick.log). Route it through the standard logger instead.
	cfg.LogOutput = nil
	cfg.Logger = yamuxLogger{}
	return cfg
}

// yamuxLogQuiet, while set, drops yamux's own log output. The sync TUI sets
// it for as long as it's on screen: every yamux error there ends the session
// and is already reported, more readably, by connectAgentWithReconnect's
// "Brick CLI disconnected (...); reconnecting..." line.
var yamuxLogQuiet atomic.Bool

// yamuxLogger forwards yamux's log output to the standard logger unless
// yamuxLogQuiet is set. It checks the flag on every call, not once per
// session, so a long-lived session follows the TUI starting or stopping.
type yamuxLogger struct{}

func (yamuxLogger) Print(v ...any) {
	if !yamuxLogQuiet.Load() {
		log.Print(v...)
	}
}

func (yamuxLogger) Printf(format string, v ...any) {
	if !yamuxLogQuiet.Load() {
		log.Printf(format, v...)
	}
}

func (yamuxLogger) Println(v ...any) {
	if !yamuxLogQuiet.Load() {
		log.Println(v...)
	}
}

// stringList is a repeatable / comma-separated string flag (e.g. --agent-root).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// agentRootsFlag collects --agent-root values; registered in main().
var agentRootsFlag stringList

// --- Local agent HTTP API ---

// agentRoot is one directory remote control may access. A read-only root
// rejects every endpoint that would change anything inside it.
type agentRoot struct {
	path     string // absolute, cleaned
	readOnly bool
}

type agentServer struct {
	roots         []agentRoot
	secret        string
	sc            *storageClient
	remoteControl bool
	// ctx is the sync session's context. Async transfers run under it rather
	// than their request's, so they outlive the request but stop when brick
	// shuts down.
	ctx       context.Context
	transfers *transferRegistry
	// settings reads and changes the running sync's exclude list; nil when
	// there's no sync to apply a change to.
	settings excludeDirsSettings
}

// excludeDirsSettings is how the agent reads and changes the active account's
// excludeDirs: persisted to config.yaml and applied to the running sync
// engine (see syncExcludeDirsSettings in sync.go).
type excludeDirsSettings interface {
	excludeDirs() []string
	setExcludeDirs(dirs []string) error
}

type fsEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "file" or "dir"
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// startAgentServer binds a loopback listener and serves the agent API. The
// caller closes the returned listener to stop it. historyLimit caps the
// finished-transfer history (<= 0 means defaultTransferHistoryLimit).
func startAgentServer(ctx context.Context, roots []agentRoot, secret string, sc *storageClient, remoteControl bool, historyLimit int, settings excludeDirsSettings) (*agentServer, net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	a := newAgentServer(ctx, roots, secret, sc, remoteControl, historyLimit)
	a.settings = settings
	srv := &http.Server{Handler: a.handler()}
	go srv.Serve(ln) //nolint:errcheck
	return a, ln, nil
}

func newAgentServer(ctx context.Context, roots []agentRoot, secret string, sc *storageClient, remoteControl bool, historyLimit int) *agentServer {
	return &agentServer{
		roots:         roots,
		secret:        secret,
		sc:            sc,
		remoteControl: remoteControl,
		ctx:           ctx,
		transfers:     newTransferRegistry(historyLimit),
	}
}

func (a *agentServer) handler() http.Handler {
	mux := http.NewServeMux()
	// /version only needs the secret, not remote control, so Brick HQ can
	// show which CLI version a device runs even when remote control is off.
	mux.HandleFunc("/version", a.authSecret(a.handleVersion))
	mux.HandleFunc("/fs/roots", a.auth(a.handleRoots))
	mux.HandleFunc("/fs/list", a.auth(a.handleList))
	mux.HandleFunc("/fs/stat", a.auth(a.handleStat))
	mux.HandleFunc("/fs/read", a.auth(a.handleRead))
	mux.HandleFunc("/fs/write", a.auth(a.handleWrite))
	mux.HandleFunc("/fs/mkdir", a.auth(a.handleMkdir))
	mux.HandleFunc("/fs/delete", a.auth(a.handleDelete))
	mux.HandleFunc("/fs/move", a.auth(a.handleMove))
	mux.HandleFunc("/fs/copy", a.auth(a.handleCopy))
	mux.HandleFunc("/transfer/upload", a.auth(a.handleUpload))
	mux.HandleFunc("/transfer/download", a.auth(a.handleDownload))
	mux.HandleFunc("/transfers", a.auth(a.handleTransfers))
	mux.HandleFunc("GET /transfers/{id}", a.auth(a.handleTransfer))
	mux.HandleFunc("DELETE /transfers/{id}", a.auth(a.handleCancelTransfer))
	mux.HandleFunc("GET /config/exclude-dirs", a.auth(a.handleGetExcludeDirs))
	mux.HandleFunc("PUT /config/exclude-dirs", a.auth(a.handleSetExcludeDirs))
	return mux
}

// auth guards an endpoint that touches the local filesystem or storage: it
// needs both the agent secret and remote control enabled.
func (a *agentServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return a.authSecret(func(w http.ResponseWriter, r *http.Request) {
		if !a.remoteControl {
			writeError(w, http.StatusForbidden, "remote_control_disabled", "remote control is not enabled on this client")
			return
		}
		h(w, r)
	})
}

// authSecret only checks the agent secret, for endpoints that stay available
// with remote control disabled.
func (a *agentServer) authSecret(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.secret == "" || r.Header.Get(agentSecretHeader) != a.secret {
			writeError(w, http.StatusForbidden, "forbidden", "missing or invalid agent secret")
			return
		}
		h(w, r)
	}
}

func (a *agentServer) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":       Version,
		"buildTime":     BuildTime,
		"gitCommit":     GitCommit,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"remoteControl": a.remoteControl,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is a structured error carrying a stable machine-readable code and the
// HTTP status to respond with. Returned by helpers so handlers can surface it
// verbatim via writeErr.
type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

// writeError emits the predefined JSON error envelope: {"error":{"code","message"}}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{"code": code, "message": message},
	})
}

// writeErr emits err as a JSON error, preferring an *apiError's status/code and
// falling back to a generic internal error otherwise.
func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.Status, ae.Code, ae.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// writeOSError classifies a filesystem error into the JSON error envelope.
func writeOSError(w http.ResponseWriter, err error) {
	switch {
	case os.IsNotExist(err):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case os.IsPermission(err):
		writeError(w, http.StatusForbidden, "permission_denied", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "io_error", err.Error())
	}
}

// evalSafe resolves symlinks on the longest existing prefix of p so that the
// containment check below cannot be defeated by a symlinked leaf or parent.
func evalSafe(p string) string {
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	if rp, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(rp, filepath.Base(p))
	}
	return p
}

// resolveSafe expands ~, makes p absolute, and verifies it stays within one of
// the allowed roots. An empty path defaults to the first root.
func (a *agentServer) resolveSafe(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		if len(a.roots) == 0 {
			return "", newAPIError(http.StatusForbidden, "no_roots_configured", "no allowed roots configured")
		}
		return a.roots[0].path, nil
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", newAPIError(http.StatusBadRequest, "invalid_path", err.Error())
	}
	abs = filepath.Clean(abs)
	if _, ok := a.rootOf(abs); !ok {
		return "", newAPIError(http.StatusForbidden, "path_outside_roots", "path is outside the allowed roots")
	}
	return abs, nil
}

// resolveWritable is resolveSafe for a path an endpoint is about to change:
// it additionally rejects a path inside a read-only root.
func (a *agentServer) resolveWritable(p string) (string, error) {
	abs, err := a.resolveSafe(p)
	if err != nil {
		return "", err
	}
	if root, _ := a.rootOf(abs); root.readOnly {
		return "", newAPIError(http.StatusForbidden, "read_only_root", "path is inside a read-only root")
	}
	return abs, nil
}

// rootOf returns the root abs lies within. When roots nest, the innermost
// one wins, so a writable folder inside a read-only root (or the reverse)
// gets its own setting.
func (a *agentServer) rootOf(abs string) (agentRoot, bool) {
	real := evalSafe(abs)
	var best agentRoot
	bestLen := -1
	for _, root := range a.roots {
		rootReal := evalSafe(root.path)
		// prefix carries a trailing separator so "/a/b" doesn't match "/a/bc";
		// for the filesystem root ("/") rootReal already ends in a separator, so
		// guard against producing "//" which would reject every child path.
		prefix := rootReal
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if (real == rootReal || strings.HasPrefix(real, prefix)) && len(rootReal) > bestLen {
			best, bestLen = root, len(rootReal)
		}
	}
	return best, bestLen >= 0
}

// isRoot reports whether abs is exactly one of the configured roots (which must
// not be deleted or moved).
func (a *agentServer) isRoot(abs string) bool {
	real := evalSafe(abs)
	for _, root := range a.roots {
		if real == evalSafe(root.path) {
			return true
		}
	}
	return false
}

func (a *agentServer) resolveParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	abs, err := a.resolveSafe(r.URL.Query().Get(key))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return abs, true
}

// resolveWritableParam is resolveParam for a path the endpoint will change.
func (a *agentServer) resolveWritableParam(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	abs, err := a.resolveWritable(r.URL.Query().Get(key))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return abs, true
}

type rootEntry struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"readOnly"`
}

// handleRoots lists the allowed roots so a client can discover what it may
// navigate. Each root's path is a valid value for the "path" param of the other
// endpoints.
func (a *agentServer) handleRoots(w http.ResponseWriter, r *http.Request) {
	roots := make([]rootEntry, 0, len(a.roots))
	for _, root := range a.roots {
		roots = append(roots, rootEntry{Path: root.path, Name: filepath.Base(root.path), ReadOnly: root.readOnly})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"roots": roots})
}

// handleList serves /fs/list. Its optional query params match the Brick
// API's GET /nodes/{id}/children: sort (name, sizeBytes or updatedAt — a
// file's modification time here) and sortOrder (asc or desc), defaulting to
// name ascending. As there, folders aren't grouped first, and names compare
// byte-wise. showHidden=false leaves out dot-files and dot-folders. limit and
// offset page through the sorted entries, and count is the total before
// paging. Unlike the Brick API, an omitted (or 0) limit returns every entry,
// as /fs/list always has.
func (a *agentServer) handleList(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveParam(w, r, "path")
	if !ok {
		return
	}
	q := r.URL.Query()
	sortField, sortOrder := q.Get("sort"), q.Get("sortOrder")
	if sortField == "" {
		sortField = "name"
	}
	if sortOrder == "" {
		sortOrder = "asc"
	}
	if sortField != "name" && sortField != "sizeBytes" && sortField != "updatedAt" {
		writeError(w, http.StatusBadRequest, "invalid_sort", "sort must be name, sizeBytes or updatedAt")
		return
	}
	if sortOrder != "asc" && sortOrder != "desc" {
		writeError(w, http.StatusBadRequest, "invalid_sort", "sortOrder must be asc or desc")
		return
	}
	showHidden := true
	if v := q.Get("showHidden"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "showHidden must be true or false")
			return
		}
		showHidden = b
	}
	limit, offset := 0, 0
	for _, p := range []struct {
		name string
		dst  *int
	}{{"limit", &limit}, {"offset", &offset}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "invalid_request", p.name+" must be a non-negative integer")
				return
			}
			*p.dst = n
		}
	}

	dirents, err := os.ReadDir(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	infos := make([]os.FileInfo, 0, len(dirents))
	for _, d := range dirents {
		if !showHidden && strings.HasPrefix(d.Name(), ".") {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}

	// Ties fall back to name, the way the Brick API falls back to node ID.
	less := func(x, y os.FileInfo) bool {
		switch sortField {
		case "sizeBytes":
			if x.Size() != y.Size() {
				return x.Size() < y.Size()
			}
		case "updatedAt":
			if !x.ModTime().Equal(y.ModTime()) {
				return x.ModTime().Before(y.ModTime())
			}
		}
		return x.Name() < y.Name()
	}
	sort.SliceStable(infos, func(i, j int) bool {
		if sortOrder == "desc" {
			return less(infos[j], infos[i])
		}
		return less(infos[i], infos[j])
	})

	count := len(infos)
	infos = infos[min(offset, count):]
	if limit > 0 && limit < len(infos) {
		infos = infos[:limit]
	}

	entries := make([]fsEntry, len(infos))
	for i, info := range infos {
		entries[i] = toEntry(info)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "entries": entries, "count": count})
}

func (a *agentServer) handleStat(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveParam(w, r, "path")
	if !ok {
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "entry": toEntry(info)})
}

func toEntry(info os.FileInfo) fsEntry {
	t := "file"
	if info.IsDir() {
		t = "dir"
	}
	return fsEntry{
		Name:    info.Name(),
		Type:    t,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339),
	}
}

func (a *agentServer) handleRead(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveParam(w, r, "path")
	if !ok {
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "is_a_directory", "path is a directory")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	defer f.Close()

	ct := mime.TypeByExtension(filepath.Ext(abs))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

func (a *agentServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveWritableParam(w, r, "path")
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeOSError(w, err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".brick-agent-*")
	if err != nil {
		writeOSError(w, err)
		return
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		writeError(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}
	tmp.Close()
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "ok": true})
}

func (a *agentServer) handleMkdir(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveWritableParam(w, r, "path")
	if !ok {
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "ok": true})
}

func (a *agentServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveWritableParam(w, r, "path")
	if !ok {
		return
	}
	if a.isRoot(abs) {
		writeError(w, http.StatusForbidden, "root_protected", "refusing to delete an allowed root")
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "ok": true})
}

type fromToReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// resolveFromTo resolves a move/copy request's paths. to is always written
// to; from only when fromWritable (a move removes it, a copy just reads it).
func (a *agentServer) resolveFromTo(w http.ResponseWriter, r *http.Request, fromWritable bool) (string, string, bool) {
	var req fromToReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return "", "", false
	}
	resolveFrom := a.resolveSafe
	if fromWritable {
		resolveFrom = a.resolveWritable
	}
	from, err := resolveFrom(req.From)
	if err != nil {
		writeErr(w, err)
		return "", "", false
	}
	to, err := a.resolveWritable(req.To)
	if err != nil {
		writeErr(w, err)
		return "", "", false
	}
	return from, to, true
}

func (a *agentServer) handleMove(w http.ResponseWriter, r *http.Request) {
	from, to, ok := a.resolveFromTo(w, r, true)
	if !ok {
		return
	}
	if a.isRoot(from) {
		writeError(w, http.StatusForbidden, "root_protected", "refusing to move an allowed root")
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		writeOSError(w, err)
		return
	}
	if err := os.Rename(from, to); err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"from": from, "to": to, "ok": true})
}

func (a *agentServer) handleCopy(w http.ResponseWriter, r *http.Request) {
	from, to, ok := a.resolveFromTo(w, r, false)
	if !ok {
		return
	}
	if err := copyPath(from, to); err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"from": from, "to": to, "ok": true})
}

// copyPath copies a file or directory tree from src to dst.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, fi.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// --- Transfer between local disk and the storage API ---
//
// Both transfer endpoints stream the file rather than holding it in memory,
// and record it in a.transfers so its progress can be polled via
// GET /transfers/{id}. Each accepts two optional fields:
//
//   - transferId: a caller-chosen UUID for the transfer, so the caller can
//     poll its progress while a synchronous request is still in flight.
//   - async: respond 202 with the transfer record right away and run the
//     transfer in the background, rather than holding the request open until
//     it finishes. Inputs are still validated before responding.
//
// DELETE /transfers/{id} cancels either kind. A cancelled synchronous
// request answers 409 transfer_canceled.

type uploadReq struct {
	LocalPath  string `json:"localPath"`
	ParentID   string `json:"parentId"`
	Name       string `json:"name"`
	TransferID string `json:"transferId"`
	Async      bool   `json:"async"`
}

func (a *agentServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	var req uploadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	abs, err := a.resolveSafe(req.LocalPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		writeOSError(w, err)
		return
	}
	if info.IsDir() {
		f.Close()
		writeError(w, http.StatusBadRequest, "is_a_directory", "path is a directory")
		return
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(abs)
	}
	ctx, cancel := a.transferContext(r, req.Async)
	id, err := a.transfers.begin(req.TransferID, transferRecord{
		Direction:  "upload",
		LocalPath:  abs,
		ParentID:   req.ParentID,
		Name:       name,
		BytesTotal: info.Size(),
	}, cancel)
	if err != nil {
		cancel()
		f.Close()
		writeErr(w, err)
		return
	}

	run := func() (*storageNode, error) {
		defer f.Close()
		node, err := a.sc.uploadStream(ctx, req.ParentID, name, f, info.Size(), func(written int64) {
			a.transfers.progress(id, written)
		})
		a.transfers.finish(ctx, id, node, err)
		return node, err
	}
	if req.Async {
		go run() //nolint:errcheck // the outcome is recorded in a.transfers
		a.writeTransferAccepted(w, id)
		return
	}
	node, err := run()
	if err != nil {
		if a.transfers.canceled(id) {
			writeTransferCanceled(w)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	rec, _ := a.transfers.get(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": node, "transfer": rec})
}

type downloadReq struct {
	NodeID     string `json:"nodeId"`
	LocalPath  string `json:"localPath"`
	TransferID string `json:"transferId"`
	Async      bool   `json:"async"`
}

func (a *agentServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req downloadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	abs, err := a.resolveWritable(req.LocalPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Fetched first for the file's size (a chunked download response may not
	// carry a Content-Length), which progress is reported against.
	node, err := a.sc.getNode(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if node.NodeType != "file" {
		writeError(w, http.StatusBadRequest, "not_a_file", "node is not a file")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeOSError(w, err)
		return
	}
	ctx, cancel := a.transferContext(r, req.Async)
	id, err := a.transfers.begin(req.TransferID, transferRecord{
		Direction:  "download",
		LocalPath:  abs,
		NodeID:     node.ID,
		Name:       node.Name,
		BytesTotal: node.SizeBytes,
	}, cancel)
	if err != nil {
		cancel()
		writeErr(w, err)
		return
	}

	run := func() error {
		err := a.sc.downloadToFile(ctx, node.ID, abs, node.SizeBytes, func(written, _ int64) {
			a.transfers.progress(id, written)
		})
		a.transfers.finish(ctx, id, nil, err)
		return err
	}
	if req.Async {
		go run() //nolint:errcheck // the outcome is recorded in a.transfers
		a.writeTransferAccepted(w, id)
		return
	}
	if err := run(); err != nil {
		if a.transfers.canceled(id) {
			writeTransferCanceled(w)
			return
		}
		var pathErr *os.PathError
		var linkErr *os.LinkError
		if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
			writeOSError(w, err)
		} else {
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		}
		return
	}
	rec, _ := a.transfers.get(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "ok": true, "transfer": rec})
}

// transferContext returns the context a transfer runs under, cancellable via
// DELETE /transfers/{id}. A synchronous transfer is also tied to its request;
// an async one outlives the request and stops only when brick shuts down.
func (a *agentServer) transferContext(r *http.Request, async bool) (context.Context, context.CancelFunc) {
	if async {
		return context.WithCancel(a.ctx)
	}
	return context.WithCancel(r.Context())
}

func writeTransferCanceled(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "transfer_canceled", "the transfer was canceled")
}

// writeTransferAccepted answers an async transfer request with the
// transfer's current record.
func (a *agentServer) writeTransferAccepted(w http.ResponseWriter, id string) {
	rec, _ := a.transfers.get(id)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"transfer": rec})
}

// --- Persistent connection to the storage API ---

// toWSURL rewrites an http(s) base URL to its ws(s) equivalent.
func toWSURL(serverURL string) string {
	switch {
	case strings.HasPrefix(serverURL, "https://"):
		return "wss://" + serverURL[len("https://"):]
	case strings.HasPrefix(serverURL, "http://"):
		return "ws://" + serverURL[len("http://"):]
	}
	return serverURL
}

// reconnectBackoff is the sequence of delays between agent reconnect attempts.
// The last value is reused for all subsequent attempts.
var reconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

// wsConn wraps *websocket.Conn as an io.ReadWriter so io.Copy (via yamux) can
// drive it.
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
	mu     sync.Mutex // serializes all WebSocket writes
}

func newWSConn(ws *websocket.Conn) *wsConn {
	return &wsConn{ws: ws}
}

func (c *wsConn) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.PingMessage, nil)
}

func (c *wsConn) Read(b []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if err == io.EOF {
				c.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		msgType, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		if msgType == websocket.CloseMessage {
			return 0, io.EOF
		}
		c.reader = r
	}
}

func (c *wsConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsConn) Close() error { return c.ws.Close() }

// closeGracefully sends a proper WebSocket close control frame before the
// connection is torn down, so the storage API sees an intentional disconnect
// (code 1000 "normal closure") instead of the raw TCP drop that
// yamux.Session.Close()/wsConn.Close() alone produces — which the server logs
// as an alarming-looking "close 1006 (abnormal closure): unexpected EOF",
// even on an entirely ordinary Ctrl-C. Bounded by a short write deadline so a
// slow or already-dead peer can never block shutdown on it. Guarded by the
// same mutex as every other write: gorilla's *websocket.Conn forbids
// concurrent writers, and both the ping goroutine and yamux (via Write) write
// to this same connection.
func (c *wsConn) closeGracefully() {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "client shutting down")
	_ = c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
}

// proxyAgentStream reads a single HTTP request off stream, forwards it to the
// local agent HTTP API at agentAddr, and writes the response back. The storage
// API opens one yamux stream per proxied request, so this handles exactly one
// request/response pair before returning.
func proxyAgentStream(stream net.Conn, agentAddr string) {
	defer stream.Close()
	start := time.Now()

	req, err := http.ReadRequest(bufio.NewReader(stream))
	if err != nil {
		log.Printf("Failed to read request: %v", err)
		return
	}

	localConn, err := net.Dial("tcp", agentAddr)
	if err != nil {
		log.Printf("Local dial failed (%s): %v", agentAddr, err)
		return
	}
	defer localConn.Close()

	req.Host = agentAddr
	if err := req.Write(localConn); err != nil {
		log.Printf("Failed to forward request: %v", err)
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(localConn), req)
	if err != nil {
		log.Printf("Failed to read response: %v", err)
		return
	}
	defer resp.Body.Close()

	if err := resp.Write(stream); err != nil {
		log.Printf("Failed to write response: %v", err)
		return
	}

	log.Printf("Brick CLI: %s %s %d %s", req.Method, req.URL.RequestURI(), resp.StatusCode, time.Since(start).Round(time.Millisecond))
}

// connectAgentOnce dials the storage API, takes the yamux server role, and
// proxies incoming streams to the local agent until the session ends or ctx is
// cancelled. It reads cfg.AccessToken fresh so reconnects pick up refreshed
// tokens, and refreshes once on a 401/403. onConnected, if non-nil, is called
// once the connection is actually established (right after the "Brick CLI
// connected..." log line) — see connectAgentWithReconnect's doc comment for
// why.
func connectAgentOnce(ctx context.Context, storageURL, apiURL string, cfg *Config, secret, agentAddr string, remoteControl bool, onConnected func()) error {
	hostname, _ := os.Hostname()
	q := url.Values{}
	q.Set("clientId", cfg.ClientID)
	q.Set("hostname", hostname)
	q.Set("os", runtime.GOOS)
	q.Set("arch", runtime.GOARCH)
	q.Set("remoteControl", strconv.FormatBool(remoteControl))
	muxURL := toWSURL(storageURL) + "/v1/accounts/" + url.PathEscape(cfg.ActiveAccountID) + "/agent/connect?" + q.Encode()

	presented := currentAccessToken(cfg)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+presented)
	hdr.Set(agentSecretHeader, secret)

	ws, resp, err := websocket.DefaultDialer.Dial(muxURL, hdr)
	if err != nil {
		// On a failed handshake gorilla returns the HTTP response with an open
		// body; close it so repeated reconnects don't leak file descriptors.
		if resp != nil {
			resp.Body.Close()
		}
		// The Storage API may report an expired token as 403 (it proxies token
		// validation to the auth API and historically collapsed a 401 there into
		// a 403), so refresh on both statuses; the caller reconnects with the
		// refreshed token. The refresh is serialized with the sync engine's so a
		// concurrent reconnect can't double-spend the single-use refresh token;
		// if another goroutine already rotated it, this adopts the new token.
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && cfg.RefreshToken != "" {
			_, _ = rotateAccessToken(apiURL, cfg, presented)
			return fmt.Errorf("Dial rejected (HTTP %d); refreshed token, will retry", resp.StatusCode)
		}
		if resp != nil {
			return fmt.Errorf("Dial failed (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("Dial failed: %w", err)
	}
	defer ws.Close()

	conn := newWSConn(ws)
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		return fmt.Errorf("Connection session failed: %w", err)
	}
	defer session.Close()

	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := conn.ping(); err != nil {
					return
				}
			}
		}
	}()

	log.Printf("Brick CLI connected to Brick Online (clientId=%s)", cfg.ClientID)
	if onConnected != nil {
		onConnected()
	}

	streamCh := make(chan net.Conn)
	errCh := make(chan error, 1)
	go func() {
		for {
			stream, err := session.Accept()
			if err != nil {
				errCh <- err
				return
			}
			streamCh <- stream
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// A deliberate local shutdown (Ctrl-C, detach, ...) — send a
			// proper close frame before the deferred session.Close()/ws.Close()
			// above drop the raw connection, so the server logs a clean
			// disconnect rather than an abnormal-closure error. Only this
			// branch does: the errCh case below means the connection is
			// already gone, with nothing left to send a close frame over.
			conn.closeGracefully()
			return nil
		case err := <-errCh:
			return fmt.Errorf("Session closed: %w", err)
		case stream := <-streamCh:
			go proxyAgentStream(stream, agentAddr)
		}
	}
}

// connectAgentWithReconnect keeps the agent connected, retrying with backoff
// until ctx is cancelled. Unlike a tunnel's reconnect logic it never gives up,
// since the agent is a long-lived background service alongside sync.
//
// connected, if non-nil, is closed the first time a connection actually
// succeeds — runSyncLoop gives it a short bounded wait right after starting
// this goroutine so "Brick CLI connected..." reliably lands ahead of the
// initial reconcile pass's own output in the common case (a fast local
// network), without letting a slow or unreachable connection delay sync
// startup by more than that bound; this goroutine's own retry loop keeps
// going regardless of whether anyone was waiting.
func connectAgentWithReconnect(ctx context.Context, storageURL, apiURL string, cfg *Config, secret, agentAddr string, remoteControl bool, connected chan<- struct{}) {
	var signalOnce sync.Once
	signalConnected := func() {
		if connected != nil {
			signalOnce.Do(func() { close(connected) })
		}
	}

	attempt := 0
	for {
		err := connectAgentOnce(ctx, storageURL, apiURL, cfg, secret, agentAddr, remoteControl, signalConnected)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		delay := reconnectBackoff[min(attempt, len(reconnectBackoff)-1)]
		log.Printf("Brick CLI disconnected (%v); reconnecting in %v...", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		attempt++
	}
}

// deregisterAgent best-effort removes this client from the storage API on clean
// shutdown. An unclean exit is handled server-side when the connection drops.
// Deliberately uses a fresh context rather than the sync loop's — that one is
// already cancelled by the time this deferred call runs, and this request
// must still go out for the shutdown to be clean.
func deregisterAgent(storageURL, apiURL string, cfg *Config) {
	path := "/v1/accounts/" + cfg.ActiveAccountID + "/clients/" + cfg.ClientID
	if resp, err := authedRequest(context.Background(), storageURL, apiURL, "DELETE", path, nil, nil, cfg); err == nil {
		resp.Body.Close()
	}
}

// resolveAgentRoots returns the absolute, de-duplicated set of roots the agent
// may access: the --agent-root CLI flags plus cfg.AgentRoots. The sync folder
// itself is not implicitly included — only directories explicitly configured
// for remote control are exposed. Flag roots are always read-only; a config
// root is read-only unless it sets readOnly: false. A path listed more than
// once is read-only if any listing says so.
func resolveAgentRoots(cfg *Config) []agentRoot {
	raw := make([]AgentRoot, 0, len(agentRootsFlag)+len(cfg.AgentRoots))
	for _, p := range agentRootsFlag {
		raw = append(raw, AgentRoot{Path: p})
	}
	raw = append(raw, cfg.AgentRoots...)

	index := map[string]int{}
	var out []agentRoot
	add := func(p string, readOnly bool) {
		if strings.HasPrefix(p, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		abs = filepath.Clean(abs)
		if i, ok := index[abs]; ok {
			out[i].readOnly = out[i].readOnly || readOnly
			return
		}
		index[abs] = len(out)
		out = append(out, agentRoot{path: abs, readOnly: readOnly})
	}
	for _, r := range raw {
		add(r.Path, r.isReadOnly())
	}
	return out
}

// formatAgentRoots renders roots for the "Remote control enabled" log line.
func formatAgentRoots(roots []agentRoot) string {
	parts := make([]string, len(roots))
	for i, r := range roots {
		mode := "read-write"
		if r.readOnly {
			mode = "read-only"
		}
		parts[i] = fmt.Sprintf("%s (%s)", r.path, mode)
	}
	return strings.Join(parts, ", ")
}

// newAgentSecret returns a fresh random secret for the agent session.
func newAgentSecret() string { return uuid.NewString() }
