package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// rcFolderLabel stands in for the sync folder in the TUI's top row during
// `brick rc`, since there is no folder being synced.
const rcFolderLabel = "Remote control only (not syncing)"

// runRCCmd handles `brick rc [flags]`: the same start-up as `brick sync`
// (login, instance lock, the log UI), but with remote control always on and
// no sync engine at all — no sync folder is read, watched or written, and the
// only activity is the remote control requests the agent serves and logs.
func runRCCmd(args []string, noUpgradeCheck bool) {
	rcFlags := flag.NewFlagSet("rc", flag.ExitOnError)
	rcFlags.Var(&agentRootsFlag, "agent-root", "")
	_ = rcFlags.Parse(args)
	if extra := rcFlags.Args(); len(extra) > 0 {
		fmt.Fprintf(os.Stderr, "brick rc: unexpected arguments %q\n", strings.Join(extra, " "))
		os.Exit(1)
	}

	if !noUpgradeCheck && !isRunningInDevelopment() {
		checkForUpdates()
	}
	apiURL := resolveAPIURL()
	storageURL := resolveStorageAPIURL()

	if err := runWithAutoRelogin(apiURL, authFailedReloginPrompt, func() error {
		return runRemoteControlOnly(apiURL, storageURL)
	}); err != nil {
		if errors.Is(err, errLoginDeclined) {
			return
		}
		log.Fatalf("Remote control failed: %v", err)
	}
}

// runRemoteControlOnly takes the instance lock, as a sync does — the agent
// registers under the same clientId, so a sync and an rc session running side
// by side would fight over that registration — then logs in and serves
// remote control until interrupted.
func runRemoteControlOnly(apiURL, storageURL string) error {
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

	var cfg *Config
	err = retryWhileUnreachable(func() (err error) {
		cfg, err = ensureAuthenticated(apiURL, nil)
		return err
	})
	if err != nil {
		return err
	}
	if cfg.ActiveAccountID == "" {
		return errors.New("no active account selected; run 'brick switch-accounts' first")
	}
	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}
	return runRemoteControlLoop(cfg, sc)
}

// runRemoteControlLoop is runSyncLoop with everything sync-related taken
// out: it sets up the same logging (brick.log, and the TUI when attached to a
// terminal), starts the agent with remote control enabled and keeps it
// connected, and waits for Ctrl+C/SIGTERM.
func runRemoteControlLoop(cfg *Config, sc *storageClient) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		cancel()
	}()

	interactive := term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
	if !interactive {
		fmt.Printf("I'm %sWebbite Brick CLI%s v%s, handling remote control requests (not syncing). Press Ctrl+C to stop.\n", ansiYellow, ansiReset, Version)
	}

	var syncLog *syncLogWriter
	if sl, slErr := newSyncLogWriter(); slErr != nil {
		log.Printf("could not open %s: %v", syncLogFileName, slErr)
	} else {
		syncLog = sl
		defer syncLog.Close()
	}
	baseLogOutput := io.Writer(os.Stderr)
	if syncLog != nil {
		baseLogOutput = io.MultiWriter(os.Stderr, syncLog)
	}
	baseLogOutput = newTimestampWriter(baseLogOutput)
	log.SetOutput(baseLogOutput)

	var prog *tea.Program
	var cleanupOnce sync.Once
	cleanupInteractive := func() {
		cleanupOnce.Do(func() {
			if prog != nil {
				prog.Quit()
				prog.Wait()
			}
			if interactive {
				log.SetOutput(baseLogOutput)
				yamuxLogQuiet.Store(false)
			}
		})
	}
	defer cleanupInteractive()

	if interactive {
		model := newRCTUIModel(Version, cancel)
		prog = tea.NewProgram(model, tea.WithAltScreen())
		tuiOutput := io.Writer(newTUILogWriter(prog))
		if syncLog != nil {
			tuiOutput = io.MultiWriter(tuiOutput, syncLog)
		}
		log.SetOutput(newTimestampWriter(tuiOutput))
		yamuxLogQuiet.Store(true)
		go func() {
			if _, runErr := prog.Run(); runErr != nil {
				cancel()
			}
		}()
		go func() {
			if q, qerr := sc.quota(ctx); qerr == nil {
				prog.Send(quotaMsg{q})
			} else if ctx.Err() == nil {
				log.Printf("could not read storage quota: %v", qerr)
			}
		}()
	}

	// No settings: with no sync running, GET/PATCH /settings answer
	// sync_not_running.
	agentRoots := resolveAgentRoots(cfg)
	agentSecret := newAgentSecret()
	_, agentLn, err := startAgentServer(ctx, agentRoots, agentSecret, sc, true, cfg.TransferHistoryLimit, nil)
	if err != nil {
		return fmt.Errorf("could not start remote file agent: %w", err)
	}
	defer agentLn.Close()
	go connectAgentWithReconnect(ctx, sc.baseURL, sc.apiURL, cfg, agentSecret, agentLn.Addr().String(), true, nil)
	defer deregisterAgent(sc.baseURL, sc.apiURL, cfg)
	if len(agentRoots) == 0 {
		log.Printf("Remote control enabled, but no folders are exposed (add --agent-root or agentRoots in the config file)")
	} else {
		log.Printf("Remote control enabled (roots: %s)", formatAgentRoots(agentRoots))
	}

	<-ctx.Done()
	cleanupInteractive()
	return nil
}
