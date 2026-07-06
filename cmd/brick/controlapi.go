package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// controlProtocolVersion guards compatibility between a brick binary and any
// client (e.g. a tray app) speaking the control API. Bump it whenever a
// change to the request/response shapes below isn't purely additive.
const controlProtocolVersion = 1

// controlSecretHeader carries the shared secret from controlDiscovery.Token.
// This is a distinct trust boundary from agentSecretHeader (agent.go), which
// gates the Storage API's remote-control tunnel rather than local control.
const controlSecretHeader = "X-Brick-Control-Secret"

// controlDiscovery is written to disk so a local client (e.g. a tray app) can
// find a running brick instance and how to talk to it. It lives in a
// per-user runtime directory, not the yaml config, since it's ephemeral and
// tied to one running process.
type controlDiscovery struct {
	PID             int       `json:"pid"`
	Version         string    `json:"version"`
	ProtocolVersion int       `json:"protocolVersion"`
	Transport       string    `json:"transport"` // always "unix" — see startControlServer
	Address         string    `json:"address"`
	Token           string    `json:"token"`
	StartedAt       time.Time `json:"startedAt"`
}

// controlRuntimeDir returns the per-user directory that holds the control
// socket and discovery file, creating it (mode 0700) if needed.
func controlRuntimeDir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "linux":
		if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
			base = filepath.Join(v, "brick")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "Library", "Application Support", "brick", "run")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			base = filepath.Join(v, "brick", "run")
		}
	}
	if base == "" {
		// Fallback shared by any OS/environment missing the vars above.
		cfgPath, err := configPath()
		if err != nil {
			return "", err
		}
		base = filepath.Join(filepath.Dir(cfgPath), "run")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("could not create control runtime dir: %w", err)
	}
	return base, nil
}

func controlDiscoveryPath() (string, error) {
	dir, err := controlRuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.json"), nil
}

func newControlToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// controlServer is the local HTTP-over-unix-socket control plane a tray app
// (or any other local client) uses to read sync status and issue commands.
// It never binds a network-reachable address.
type controlServer struct {
	ln            net.Listener
	socketPath    string
	discoveryPath string
	token         string
	httpSrv       *http.Server
}

// startControlServer binds a unix-domain-socket listener in the control
// runtime dir, writes the discovery file, and starts serving. cancel is
// called to trigger the same graceful shutdown path SIGINT uses; notify is
// called to wake the debounced reconcile worker immediately (used by
// /v1/resume so resuming doesn't wait for the next watcher event or poll).
func startControlServer(eng *syncEngine, cancel context.CancelFunc, notify func()) (*controlServer, error) {
	dir, err := controlRuntimeDir()
	if err != nil {
		return nil, err
	}
	socketPath := filepath.Join(dir, "control.sock")

	// A stale socket file left behind by a crashed process would otherwise
	// make Listen fail with "address already in use". The instance lock
	// (acquired before this is ever called) guarantees we're the only brick
	// process, so it's always safe to clear the way here.
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("could not listen on control socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	token, err := newControlToken()
	if err != nil {
		ln.Close()
		return nil, err
	}

	discoveryPath, err := controlDiscoveryPath()
	if err != nil {
		ln.Close()
		return nil, err
	}
	disc := controlDiscovery{
		PID:             os.Getpid(),
		Version:         Version,
		ProtocolVersion: controlProtocolVersion,
		Transport:       "unix",
		Address:         socketPath,
		Token:           token,
		StartedAt:       time.Now().UTC(),
	}
	discJSON, err := json.MarshalIndent(disc, "", "  ")
	if err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.WriteFile(discoveryPath, discJSON, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("could not write discovery file: %w", err)
	}

	cs := &controlServer{ln: ln, socketPath: socketPath, discoveryPath: discoveryPath, token: token}
	cs.httpSrv = &http.Server{Handler: cs.handler(eng, cancel, notify)}
	go cs.httpSrv.Serve(ln) //nolint:errcheck

	return cs, nil
}

// Close stops serving and removes the socket and discovery files so a
// leftover file never points a client at a dead process.
func (cs *controlServer) Close() {
	cs.httpSrv.Close() //nolint:errcheck
	_ = os.Remove(cs.socketPath)
	_ = os.Remove(cs.discoveryPath)
}

func (cs *controlServer) handler(eng *syncEngine, cancel context.CancelFunc, notify func()) http.Handler {
	mux := http.NewServeMux()

	// /v1/health needs no token: reachability of the unix socket (which is
	// already 0600, in a 0700 directory) is the only thing it attests to.
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "protocolVersion": controlProtocolVersion})
	})

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			presented := r.Header.Get(controlSecretHeader)
			if subtle.ConstantTimeCompare([]byte(presented), []byte(cs.token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/v1/status", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, eng.statusSnapshot())
	}))

	mux.HandleFunc("/v1/activity", authed(func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := parsePositiveInt(v); err == nil {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, eng.recentActivity(limit))
	}))

	mux.HandleFunc("/v1/account", authed(func(w http.ResponseWriter, r *http.Request) {
		accountID, clientID := "", ""
		if eng.sc != nil && eng.sc.cfg != nil {
			accountID = eng.sc.cfg.AccountID
			clientID = eng.sc.cfg.ClientID
		}
		writeJSON(w, http.StatusOK, map[string]any{"accountId": accountID, "clientId": clientID})
	}))

	mux.HandleFunc("/v1/pause", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		eng.setPaused(true)
		writeJSON(w, http.StatusOK, eng.statusSnapshot())
	}))

	mux.HandleFunc("/v1/resume", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		eng.setPaused(false)
		notify() // wake the debounce worker immediately instead of waiting on the next event/tick
		writeJSON(w, http.StatusOK, eng.statusSnapshot())
	}))

	mux.HandleFunc("/v1/quit", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		cancel()
	}))

	return mux
}

func parsePositiveInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n, nil
}
