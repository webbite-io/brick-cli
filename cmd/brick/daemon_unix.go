//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// daemonSupported reports whether this platform can run brick as a detached
// background daemon (see runAsDaemon); used to decide whether the
// interactive sync banner advertises the 'D' detach shortcut at all.
const daemonSupported = true

// daemonLogPath returns the file the detached daemon child's stdout/stderr
// are redirected to, since it no longer has a controlling terminal.
func daemonLogPath() (string, error) {
	cfgPath, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "daemon.log"), nil
}

// runAsDaemon runs every interactive step of starting a sync (login,
// sync-folder selection, first-run onboarding) attached to the current
// terminal, exactly like a normal foreground run. Only once that succeeds —
// meaning brick is logged in and the Storage API is reachable — does it
// re-exec itself as a detached background process to perform the actual
// sync, then return control to the caller's shell.
func runAsDaemon(apiURL, storageURL string, remoteControl, noControlAPI bool) error {
	pid, logPath, _, err := startDaemonProcess(apiURL, storageURL, remoteControl, noControlAPI, filterDaemonArgs(os.Args[1:]))
	if err != nil {
		if errors.Is(err, errInstanceLocked) {
			return errors.New("brick is already running for this user")
		}
		return err
	}

	fmt.Printf("brick is now syncing in the background (pid %d).\n", pid)
	fmt.Printf("Logs: %s\n", logPath)
	return nil
}

// runAsDaemonJSON is the --json counterpart to runAsDaemon, for a companion
// app that starts brick in daemon mode without a terminal to prompt on. It
// never runs any interactive step: if login, account selection or sync-folder
// setup haven't already been completed via a prior interactive run, it
// reports "setup_required" instead of falling back to onboarding. Exactly one
// line of JSON is printed to stdout and the process exits from within this
// function (via emitDaemonJSON) — it never returns.
func runAsDaemonJSON(apiURL, storageURL string, remoteControl, noControlAPI bool) {
	cfg, _, err := loadOrCreateConfigQuiet()
	if err != nil {
		emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "internal_error", Message: err.Error()})
		return
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "setup_required", Message: "brick is not logged in; run 'brick --login' first"})
		return
	}
	if cfg.ActiveAccountID == "" {
		emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "setup_required", Message: "no active account selected; run 'brick --switch-accounts' first"})
		return
	}
	ac := cfg.activeAccount()
	if ac == nil || strings.TrimSpace(ac.StorageSyncFolder) == "" {
		emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "setup_required", Message: "no sync folder configured; run 'brick' interactively first to complete setup"})
		return
	}

	pid, logPath, folder, err := startDaemonProcess(apiURL, storageURL, remoteControl, noControlAPI, filterDaemonArgs(os.Args[1:]))
	if err != nil {
		if errors.Is(err, errInstanceLocked) {
			emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "already_running", Message: "brick is already running for this user"})
			return
		}
		emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "start_failed", Message: err.Error()})
		return
	}
	emitDaemonJSON(daemonJSONOutput{Status: "ok", PID: pid, LogPath: logPath, Folder: folder})
}

// startDaemonProcess contains the mechanics shared by runAsDaemon and
// runAsDaemonJSON: run prepareSync (a no-op beyond a reachability check once
// login/account/folder are already configured), then re-exec the binary
// detached in the background, handing over the folder/conflict-mode/
// first-setup decisions to the child via env vars (see runDaemonChild).
//
// The instance lock is acquired here and handed to the detached child via
// ExtraFiles (inherited as fd 3) rather than released and re-acquired, so no
// other brick invocation can slip in and grab it during the handoff.
//
// cliArgs is the argument list the detached child re-execs itself with (see
// filterDaemonArgs); callers starting a daemon from the current process's own
// invocation pass filterDaemonArgs(os.Args[1:]), while relaunchDaemon builds
// an explicit list from previously persisted flags instead.
func startDaemonProcess(apiURL, storageURL string, remoteControl, noControlAPI bool, cliArgs []string) (pid int, logPath, folder string, err error) {
	lockPath, err := instanceLockPath()
	if err != nil {
		return 0, "", "", err
	}
	lock, err := acquireInstanceLock(lockPath)
	if err != nil {
		return 0, "", "", err
	}

	setup, err := prepareSync(apiURL, storageURL)
	if err != nil {
		lock.Release()
		return 0, "", "", err
	}

	execPath, err := os.Executable()
	if err != nil {
		lock.Release()
		return 0, "", "", fmt.Errorf("could not determine binary path: %w", err)
	}

	logPath, err = daemonLogPath()
	if err != nil {
		lock.Release()
		return 0, "", "", err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		lock.Release()
		return 0, "", "", fmt.Errorf("could not open daemon log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(execPath, cliArgs...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{lock.f}
	cmd.Env = append(os.Environ(),
		daemonFolderEnv+"="+setup.folder,
		daemonConflictModeEnv+"="+setup.conflictMode,
		daemonFirstSetupEnv+"="+firstSetupEnvValue(setup.isFirstSetup),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		lock.Release()
		return 0, "", "", fmt.Errorf("could not start daemon process: %w", err)
	}
	// The child now shares the lock via its inherited copy of fd 3; drop ours
	// with a plain Close (not Release, which would also unlock) so the flock
	// stays held for the life of the daemon.
	lock.f.Close()

	return cmd.Process.Pid, logPath, setup.folder, nil
}

// filterDaemonArgs drops -d/--daemon and --json from args and ensures
// --no-upgrade-check is set, since the detached child re-runs the normal sync
// flow directly and has no terminal to prompt on for an update, and doesn't
// itself understand --json (that flag only governs how the parent reports
// the outcome of starting the child).
func filterDaemonArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	hasNoUpgradeCheck := false
	for _, a := range args {
		if a == "-d" || a == "--daemon" || a == "--json" {
			continue
		}
		if a == "--no-upgrade-check" {
			hasNoUpgradeCheck = true
		}
		out = append(out, a)
	}
	if !hasNoUpgradeCheck {
		out = append(out, "--no-upgrade-check")
	}
	return out
}

func firstSetupEnvValue(v bool) string {
	if v {
		return "1"
	}
	return ""
}

// runDaemonChild is the entry point for the detached process runAsDaemon
// spawns. Login, sync folder selection and onboarding already ran in the
// foreground parent; this adopts the instance lock inherited via fd 3 (see
// ExtraFiles above), re-derives a storageClient, re-verifies the Storage API
// is reachable, and starts the normal sync loop — carrying over the
// folder/conflictMode/isFirstSetup decisions made in the parent so the very
// first reconcile pass resolves any pre-existing conflict exactly as the
// user chose, rather than silently defaulting to "remote wins".
func runDaemonChild(apiURL, storageURL string, remoteControl, noControlAPI bool, folder, conflictMode string, isFirstSetup bool) error {
	lockPath, err := instanceLockPath()
	if err != nil {
		return err
	}
	lock := &instanceLock{f: os.NewFile(3, lockPath)}
	defer lock.Release()

	cfg, err := loadOrCreateConfig()
	if err != nil {
		return err
	}
	if cfg.ActiveAccountID == "" {
		return errors.New("no active account selected; run 'brick --switch-accounts' first")
	}

	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}
	root, err := sc.resolveRoot(context.Background())
	if err != nil {
		return fmt.Errorf("could not reach storage API at %s: %w", storageURL, err)
	}

	setup := &syncSetup{
		cfg:          cfg,
		sc:           sc,
		folder:       folder,
		rootID:       root.ID,
		conflictMode: conflictMode,
		isFirstSetup: isFirstSetup,
	}
	// background is true here, so runSyncLoop never enables interactive mode
	// and detach (its bool return) is always false.
	_, err = runSyncLoop(setup, remoteControl, noControlAPI, true)
	return err
}

// relaunchDaemon starts a fresh detached daemon reusing the remoteControl and
// agentRoots flags a previous instance was running with, used by
// restartDaemonIfRunning after 'brick --switch-accounts' stops a background
// daemon so syncing resumes automatically under the new account.
func relaunchDaemon(apiURL, storageURL string, remoteControl bool, agentRoots []string) error {
	args := []string{"--no-upgrade-check"}
	if remoteControl {
		args = append(args, "-r")
	}
	for _, root := range agentRoots {
		args = append(args, "--agent-root", root)
	}

	pid, logPath, _, err := startDaemonProcess(apiURL, storageURL, remoteControl, false, args)
	if err != nil {
		if errors.Is(err, errInstanceLocked) {
			return errors.New("brick is already running for this user")
		}
		return err
	}
	fmt.Printf("brick is syncing in the background again (pid %d).\n", pid)
	fmt.Printf("Logs: %s\n", logPath)
	return nil
}
