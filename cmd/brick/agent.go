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
	"strconv"
	"strings"
	"sync"
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
	return cfg
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

type agentServer struct {
	roots         []string // absolute, cleaned allowed roots
	secret        string
	sc            *storageClient
	remoteControl bool
}

type fsEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "file" or "dir"
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// startAgentServer binds a loopback listener and serves the agent API. The
// caller closes the returned listener to stop it.
func startAgentServer(roots []string, secret string, sc *storageClient, remoteControl bool) (*agentServer, net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	a := &agentServer{roots: roots, secret: secret, sc: sc, remoteControl: remoteControl}
	srv := &http.Server{Handler: a.handler()}
	go srv.Serve(ln) //nolint:errcheck
	return a, ln, nil
}

func (a *agentServer) handler() http.Handler {
	mux := http.NewServeMux()
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
	return mux
}

func (a *agentServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.remoteControl {
			writeError(w, http.StatusForbidden, "remote_control_disabled", "remote control is not enabled on this client")
			return
		}
		if a.secret == "" || r.Header.Get(agentSecretHeader) != a.secret {
			writeError(w, http.StatusForbidden, "forbidden", "missing or invalid agent secret")
			return
		}
		h(w, r)
	}
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
		return a.roots[0], nil
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
	real := evalSafe(abs)
	for _, root := range a.roots {
		rootReal := evalSafe(root)
		// prefix carries a trailing separator so "/a/b" doesn't match "/a/bc";
		// for the filesystem root ("/") rootReal already ends in a separator, so
		// guard against producing "//" which would reject every child path.
		prefix := rootReal
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if real == rootReal || strings.HasPrefix(real, prefix) {
			return abs, nil
		}
	}
	return "", newAPIError(http.StatusForbidden, "path_outside_roots", "path is outside the allowed roots")
}

// isRoot reports whether abs is exactly one of the configured roots (which must
// not be deleted or moved).
func (a *agentServer) isRoot(abs string) bool {
	real := evalSafe(abs)
	for _, root := range a.roots {
		if real == evalSafe(root) {
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

type rootEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// handleRoots lists the allowed roots so a client can discover what it may
// navigate. Each root's path is a valid value for the "path" param of the other
// endpoints.
func (a *agentServer) handleRoots(w http.ResponseWriter, r *http.Request) {
	roots := make([]rootEntry, 0, len(a.roots))
	for _, root := range a.roots {
		roots = append(roots, rootEntry{Path: root, Name: filepath.Base(root)})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"roots": roots})
}

func (a *agentServer) handleList(w http.ResponseWriter, r *http.Request) {
	abs, ok := a.resolveParam(w, r, "path")
	if !ok {
		return
	}
	dirents, err := os.ReadDir(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	entries := make([]fsEntry, 0, len(dirents))
	for _, d := range dirents {
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, toEntry(info))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "entries": entries})
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
	abs, ok := a.resolveParam(w, r, "path")
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
	abs, ok := a.resolveParam(w, r, "path")
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
	abs, ok := a.resolveParam(w, r, "path")
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

func (a *agentServer) resolveFromTo(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var req fromToReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return "", "", false
	}
	from, err := a.resolveSafe(req.From)
	if err != nil {
		writeErr(w, err)
		return "", "", false
	}
	to, err := a.resolveSafe(req.To)
	if err != nil {
		writeErr(w, err)
		return "", "", false
	}
	return from, to, true
}

func (a *agentServer) handleMove(w http.ResponseWriter, r *http.Request) {
	from, to, ok := a.resolveFromTo(w, r)
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
	from, to, ok := a.resolveFromTo(w, r)
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

type uploadReq struct {
	LocalPath string `json:"localPath"`
	ParentID  string `json:"parentId"`
	Name      string `json:"name"`
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
	data, err := os.ReadFile(abs)
	if err != nil {
		writeOSError(w, err)
		return
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(abs)
	}
	node, err := a.sc.upload(r.Context(), req.ParentID, name, data)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": node})
}

type downloadReq struct {
	NodeID    string `json:"nodeId"`
	LocalPath string `json:"localPath"`
}

func (a *agentServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	var req downloadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	abs, err := a.resolveSafe(req.LocalPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	data, _, err := a.sc.download(r.Context(), req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeOSError(w, err)
		return
	}
	if err := atomicWrite(abs, data); err != nil {
		writeOSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": abs, "ok": true})
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
// tokens, and refreshes once on a 401/403.
func connectAgentOnce(ctx context.Context, storageURL, apiURL string, cfg *Config, secret, agentAddr string, remoteControl bool) error {
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
func connectAgentWithReconnect(ctx context.Context, storageURL, apiURL string, cfg *Config, secret, agentAddr string, remoteControl bool) {
	attempt := 0
	for {
		err := connectAgentOnce(ctx, storageURL, apiURL, cfg, secret, agentAddr, remoteControl)
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
// for remote control are exposed.
func resolveAgentRoots(cfg *Config) []string {
	raw := append([]string{}, agentRootsFlag...)
	raw = append(raw, cfg.AgentRoots...)

	seen := map[string]bool{}
	var out []string
	add := func(p string) {
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
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	for _, r := range raw {
		add(r)
	}
	return out
}

// newAgentSecret returns a fresh random secret for the agent session.
func newAgentSecret() string { return uuid.NewString() }
