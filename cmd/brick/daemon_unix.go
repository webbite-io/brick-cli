//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

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
//
// The instance lock is acquired here and handed to the detached child via
// ExtraFiles (inherited as fd 3) rather than released and re-acquired, so no
// other brick invocation can slip in and grab it during the handoff.
func runAsDaemon(apiURL, storageURL string, remoteControl, noControlAPI bool) error {
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

	setup, err := prepareSync(apiURL, storageURL)
	if err != nil {
		lock.Release()
		return err
	}

	execPath, err := os.Executable()
	if err != nil {
		lock.Release()
		return fmt.Errorf("could not determine binary path: %w", err)
	}

	logPath, err := daemonLogPath()
	if err != nil {
		lock.Release()
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		lock.Release()
		return fmt.Errorf("could not open daemon log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(execPath, filterDaemonArgs(os.Args[1:])...)
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
		return fmt.Errorf("could not start daemon process: %w", err)
	}
	// The child now shares the lock via its inherited copy of fd 3; drop ours
	// with a plain Close (not Release, which would also unlock) so the flock
	// stays held for the life of the daemon.
	lock.f.Close()

	fmt.Printf("brick is now syncing in the background (pid %d).\n", cmd.Process.Pid)
	fmt.Printf("Logs: %s\n", logPath)
	return nil
}

// filterDaemonArgs drops -d/--daemon from args and ensures --no-upgrade-check
// is set, since the detached child re-runs the normal sync flow directly and
// has no terminal to prompt on for an update.
func filterDaemonArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	hasNoUpgradeCheck := false
	for _, a := range args {
		if a == "-d" || a == "--daemon" {
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
	if cfg.AccountID == "" {
		return errors.New("no active account selected; run 'brick --switch-accounts' first")
	}

	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.AccountID, cfg: cfg}
	root, err := sc.resolveRoot()
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
	return runSyncLoop(setup, remoteControl, noControlAPI)
}
