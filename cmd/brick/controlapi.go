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

	// Background is true when this process is a detached daemon (started via
	// -d/--daemon) rather than a foreground run. Only background instances
	// are safe to relaunch unattended after 'brick --switch-accounts' stops
	// them — a foreground run is attached to someone's terminal and is left
	// stopped instead.
	Background bool `json:"background"`
	// RemoteControl and AgentRoots mirror the flags this instance was
	// started with, so a later restart (see restartDaemonIfRunning) can
	// relaunch it identically.
	RemoteControl bool     `json:"remoteControl"`
	AgentRoots    []string `json:"agentRoots,omitempty"`
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
func startControlServer(eng *syncEngine, cancel context.CancelFunc, notify func(), background, remoteControl bool, agentRoots []string) (*controlServer, error) {
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
		Background:      background,
		RemoteControl:   remoteControl,
		AgentRoots:      agentRoots,
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
			accountID = eng.sc.cfg.ActiveAccountID
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
		// Block until any reconcile pass already in flight finishes, so a 200
		// here is a genuine guarantee that no reconcile is running or will
		// start until resume — callers that are about to touch the sync
		// folder out-of-band (e.g. runSelectiveSync) rely on that.
		eng.mu.Lock()   //nolint:staticcheck // used as a barrier, not to guard data
		eng.mu.Unlock() //nolint:staticcheck
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

// controlClientFor returns an http.Client that dials the control socket at
// address regardless of the URL host/scheme passed to it.
func controlClientFor(address string) *http.Client {
	return controlClientForTimeout(address, 5*time.Second)
}

// controlClientForTimeout is controlClientFor with a caller-chosen timeout,
// for requests that may legitimately take longer than the 5s default (e.g.
// /v1/pause, which can block behind an in-flight reconcile pass).
func controlClientForTimeout(address string, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", address)
			},
		},
		Timeout: timeout,
	}
}

// stoppedInstance describes a running brick instance that stopRunningInstance
// found and stopped, so callers can decide whether/how to relaunch it.
type stoppedInstance struct {
	Background    bool
	RemoteControl bool
	AgentRoots    []string
}

// stopRunningInstance looks for a currently running brick instance via the
// control discovery file, and if one is found, stops it gracefully via the
// control API's /v1/quit endpoint. Returns nil, nil if no instance is
// running (or the discovery file is stale/corrupt) — a no-op in that case.
func stopRunningInstance(printPrefix string) (*stoppedInstance, error) {
	discPath, err := controlDiscoveryPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(discPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read control discovery file: %w", err)
	}
	var disc controlDiscovery
	if err := json.Unmarshal(data, &disc); err != nil {
		// Corrupt/foreign file — nothing reliable to act on.
		return nil, nil
	}

	client := controlClientFor(disc.Address)

	healthReq, err := http.NewRequest(http.MethodGet, "http://unix/v1/health", nil)
	if err != nil {
		return nil, err
	}
	healthResp, err := client.Do(healthReq)
	if err != nil {
		// Stale discovery file left behind by a process that didn't exit
		// cleanly (e.g. a crash or kill -9) — nothing is actually running.
		_ = os.Remove(discPath)
		return nil, nil
	}
	healthResp.Body.Close()

	fmt.Println(printPrefix)
	quitReq, err := http.NewRequest(http.MethodPost, "http://unix/v1/quit", nil)
	if err != nil {
		return nil, err
	}
	quitReq.Header.Set(controlSecretHeader, disc.Token)
	quitResp, err := client.Do(quitReq)
	if err != nil {
		return nil, fmt.Errorf("could not stop the running brick instance: %w", err)
	}
	quitResp.Body.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(discPath); os.IsNotExist(err) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	return &stoppedInstance{
		Background:    disc.Background,
		RemoteControl: disc.RemoteControl,
		AgentRoots:    disc.AgentRoots,
	}, nil
}

// pauseRunningInstance looks for a currently running brick instance via the
// control discovery file and, if found, pauses its reconcile loop over the
// control API's /v1/pause endpoint, which blocks until any in-flight
// reconcile pass has finished before responding. Callers use this to make
// sure no reconcile can be running or start while they modify the sync
// folder out-of-band (e.g. runSelectiveSync removing a newly-excluded
// folder). Returns false if no instance is running (a no-op in that case).
func pauseRunningInstance() (bool, error) {
	discPath, err := controlDiscoveryPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(discPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("could not read control discovery file: %w", err)
	}
	var disc controlDiscovery
	if err := json.Unmarshal(data, &disc); err != nil {
		// Corrupt/foreign file — nothing reliable to act on.
		return false, nil
	}

	client := controlClientForTimeout(disc.Address, 2*time.Minute)
	pauseReq, err := http.NewRequest(http.MethodPost, "http://unix/v1/pause", nil)
	if err != nil {
		return false, err
	}
	pauseReq.Header.Set(controlSecretHeader, disc.Token)
	resp, err := client.Do(pauseReq)
	if err != nil {
		// Stale discovery file left behind by a process that didn't exit
		// cleanly — nothing is actually running.
		return false, nil
	}
	resp.Body.Close()
	return true, nil
}

// restartDaemonIfRunning is called after 'brick --switch-accounts' picks a
// new account. It looks for a currently running brick instance and, if one
// is found, stops it gracefully (so it isn't left syncing under the
// just-replaced account) and, if it was a background daemon, relaunches it
// so syncing resumes under the new account without the user needing to do it
// by hand. A foreground instance is left stopped, since it's attached to
// someone's terminal and can't be relaunched unattended. If no instance is
// running, this is a no-op.
func restartDaemonIfRunning(apiURL, storageURL string) error {
	stopped, err := stopRunningInstance("\nStopping the running brick instance so it picks up the new account...")
	if err != nil {
		return err
	}
	if stopped == nil {
		return nil
	}

	if !stopped.Background {
		fmt.Println("Stopped. It was running in the foreground — restart it manually to resume syncing.")
		return nil
	}

	fmt.Println("Restarting brick in the background with the new account...")
	return relaunchDaemon(apiURL, storageURL, stopped.RemoteControl, stopped.AgentRoots)
}
