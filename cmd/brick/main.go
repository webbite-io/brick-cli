package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env file for local development only.
	// In production builds the compile-time defaults are baked in via ldflags,
	// so we skip .env loading to prevent local files from overriding them.
	if DefaultAPIURL == "" {
		_ = godotenv.Load()
	}

	var (
		showVersion    bool
		showHelp       bool
		noUpgradeCheck bool
		noControlAPI   bool
		selfTest       bool
		setupAndExit   bool
	)

	// Global flags. These must appear before the subcommand (if any) — the
	// stdlib flag package stops parsing at the first non-flag token, so
	// e.g. `brick --no-upgrade-check sync -d` works but `brick sync -d
	// --no-upgrade-check` does not (--no-upgrade-check would be parsed as an
	// argument to the sync subcommand instead, and rejected there).
	flag.BoolVar(&showVersion, "v", false, "")
	flag.BoolVar(&showVersion, "version", false, "")
	flag.BoolVar(&showHelp, "h", false, "")
	flag.BoolVar(&showHelp, "help", false, "")
	flag.BoolVar(&noUpgradeCheck, "no-upgrade-check", false, "")
	flag.BoolVar(&noControlAPI, "no-control-api", false, "")
	// Undocumented until recently, now documented under "Other": a read-only
	// diagnostic for a companion app to check whether brick is expected to be
	// able to sync right now, without actually starting a sync. See README.
	flag.BoolVar(&selfTest, "self-test", false, "")
	// Undocumented until recently, now documented under "Other": runs every
	// interactive setup step a normal sync start would (login, sync-folder
	// selection, first-run onboarding, a Storage API reachability check),
	// then exits without ever starting a sync. See README for details.
	flag.BoolVar(&setupAndExit, "setup-and-exit", false, "")
	flag.Usage = printHelp
	flag.Parse()

	// Show version
	if showVersion {
		fmt.Printf("brick v%s\n", Version)
		if BuildTime != "unknown" {
			fmt.Printf("Built: %s\n", BuildTime)
		}
		if GitCommit != "unknown" {
			fmt.Printf("Commit: %s\n", GitCommit)
		}
		os.Exit(0)
	}

	// Show help
	if showHelp {
		printHelp()
		os.Exit(0)
	}

	// Self-test: run every readiness check and print a single JSON line,
	// without prompting, checking for updates, or starting a sync. Placed
	// ahead of any subcommand dispatch below since that can block on stdin
	// for an upgrade prompt, which a companion app calling this must never
	// hit.
	if selfTest {
		apiURL := resolveAPIURL()
		storageURL := resolveStorageAPIURL()
		emitSelfTestOutput(runSelfTest(apiURL, storageURL))
	}

	// Setup-and-exit: run every interactive step a normal sync start would —
	// login, sync-folder selection, first-run onboarding, and confirming the
	// Storage API is reachable — then exit successfully without ever
	// starting a sync.
	if setupAndExit {
		apiURL := resolveAPIURL()
		storageURL := resolveStorageAPIURL()
		if err := runWithAutoRelogin(apiURL, authFailedReloginPrompt, func() error { return runSetupAndExit(apiURL, storageURL) }); err != nil {
			if errors.Is(err, errLoginDeclined) {
				os.Exit(0)
			}
			log.Fatalf("Setup failed: %v", err)
		}
		os.Exit(0)
	}

	switch flag.Arg(0) {
	case "":
		printHelp()
		os.Exit(0)

	case "login":
		requireNoExtraArgs("login")
		apiURL := resolveAPIURL()
		if err := runLogin(apiURL, nil); err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		os.Exit(0)

	case "switch-accounts":
		requireNoExtraArgs("switch-accounts")
		apiURL := resolveAPIURL()
		storageURL := resolveStorageAPIURL()
		if err := runWithAutoRelogin(apiURL, switchAccountsReloginPrompt, func() error { return runSwitchAccounts(apiURL, storageURL) }); err != nil {
			log.Fatalf("Switch accounts failed: %v", err)
		}
		os.Exit(0)

	case "whoami":
		requireNoExtraArgs("whoami")
		apiURL := resolveAPIURL()
		if err := runWhoami(apiURL); err != nil {
			log.Fatalf("whoami failed: %v", err)
		}
		os.Exit(0)

	case "restart":
		requireNoExtraArgs("restart")
		runRestartCmd(noUpgradeCheck, noControlAPI)
		os.Exit(0)

	case "uninstall":
		requireNoExtraArgs("uninstall")
		runUninstall()
		os.Exit(0)

	case "sync":
		runSyncCmd(flag.Args()[1:], noUpgradeCheck, noControlAPI)
		os.Exit(0)

	case "upload":
		runUploadCmd(flag.Args()[1:])
		os.Exit(0)

	case "download":
		runDownloadCmd(flag.Args()[1:])
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "brick: unknown command %q\n\n", flag.Arg(0))
		printHelp()
		os.Exit(1)
	}
}

// requireNoExtraArgs exits with an error if anything follows the subcommand
// name in flag.Args() — used by the account-mgmt subcommands, none of which
// take any flags or positional arguments of their own, so e.g. `brick login
// -d` must be rejected rather than silently ignoring -d.
func requireNoExtraArgs(cmd string) {
	if extra := flag.Args()[1:]; len(extra) > 0 {
		fmt.Fprintf(os.Stderr, "brick %s: takes no flags or arguments (got %q)\n", cmd, strings.Join(extra, " "))
		os.Exit(1)
	}
}

// isRunningInDevelopment detects if the binary is running in a development environment (e.g., with Air)
func isRunningInDevelopment() bool {
	if os.Getenv("AIR_WATCH") != "" || os.Getenv("AIR_TMP_DIR") != "" {
		return true
	}
	execPath, err := os.Executable()
	if err == nil && strings.Contains(execPath, "tmp") {
		return true
	}
	if Version == "dev" {
		return true
	}
	return false
}

// getRemoteVersion fetches the latest released version from the GitHub releases API.
func getRemoteVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/webbite-io/brick-cli/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	// Strip leading 'v' to match the Version variable format.
	return strings.TrimPrefix(release.TagName, "v"), nil
}

// checkForUpdates checks if a new version is available and prompts the user to install it.
func checkForUpdates() {
	remoteVersion, err := getRemoteVersion()
	if err != nil {
		return
	}

	if remoteVersion == Version || remoteVersion == "" {
		return
	}

	fmt.Printf("\n\033[33mThere is a new version of Webbite Brick CLI available.\033[0m\n")
	fmt.Printf("You're running v%s and the new version is v%s.\n\n", Version, remoteVersion)

	if runtime.GOOS == "windows" {
		fmt.Printf("See https://github.com/webbite-io/brick-cli/ for installation details.\n\n")
		return
	}

	fmt.Print("Do you want to install (Y/n): ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("\nContinuing with current version...")
		return
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response == "" || response == "y" || response == "yes" {
		fmt.Println("\nInstalling update...")
		if err := installUpdate(); err != nil {
			fmt.Printf("\033[31mFailed to install update: %v\033[0m\n", err)
			fmt.Printf("Please visit https://github.com/webbite-io/brick-cli/ for manual installation.\n\n")
		} else {
			fmt.Println("\033[32mUpdate installed successfully!\033[0m")
			fmt.Printf("Please restart brick to use the new version.\n\n")
			os.Exit(0)
		}
	} else {
		fmt.Println("\nContinuing with current version...")
	}
	fmt.Println()
}

// installUpdate runs the installation script.
func installUpdate() error {
	cmd := exec.Command("bash", "-c", "curl -fsSL https://raw.githubusercontent.com/webbite-io/brick-cli/main/install.sh | bash")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runUninstall removes the brick binary and optionally the config directory,
// shell completions, and man page.
func runUninstall() {
	reader := bufio.NewReader(os.Stdin)
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("\033[31mCould not determine home directory: %v\033[0m\n", err)
		os.Exit(1)
	}

	// --- Binary ---
	execPath, err := os.Executable()
	if err != nil {
		fmt.Printf("\033[31mCould not determine binary path: %v\033[0m\n", err)
		os.Exit(1)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		fmt.Printf("\033[31mCould not resolve binary path: %v\033[0m\n", err)
		os.Exit(1)
	}

	fmt.Printf("Binary to remove: %s\n", execPath)
	fmt.Print("Remove binary? (Y/n): ")
	resp, _ := reader.ReadString('\n')
	resp = strings.TrimSpace(strings.ToLower(resp))
	if resp == "" || resp == "y" || resp == "yes" {
		if err := os.Remove(execPath); err != nil {
			fmt.Printf("\033[31mFailed to remove binary: %v\033[0m\n", err)
			os.Exit(1)
		}
		fmt.Println("\033[32mBinary removed.\033[0m")
	} else {
		fmt.Println("Skipped binary removal.")
	}

	// --- Config directory ---
	cfgPath, err := configPath()
	if err != nil {
		fmt.Printf("\033[31mCould not determine config path: %v\033[0m\n", err)
		os.Exit(1)
	}
	cfgDir := filepath.Dir(cfgPath)
	fmt.Printf("\nConfig directory: %s\n", cfgDir)
	fmt.Print("Remove config directory and config file? (y/N): ")
	resp, _ = reader.ReadString('\n')
	resp = strings.TrimSpace(strings.ToLower(resp))
	if resp == "y" || resp == "yes" {
		if err := os.RemoveAll(cfgDir); err != nil {
			fmt.Printf("\033[31mFailed to remove config directory: %v\033[0m\n", err)
			os.Exit(1)
		}
		fmt.Println("\033[32mConfig directory removed.\033[0m")
	} else {
		fmt.Println("Skipped config directory removal.")
	}

	// --- Shell completions ---
	completionFiles := []string{
		filepath.Join(home, ".config", "fish", "completions", "brick.fish"),
		filepath.Join(home, ".local", "share", "bash-completion", "completions", "brick"),
		filepath.Join(home, ".local", "share", "zsh", "site-functions", "_brick"),
	}
	// On macOS, also check the Homebrew completion directories.
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("brew", "--prefix").Output(); err == nil {
			brewPrefix := strings.TrimSpace(string(out))
			completionFiles = append(completionFiles,
				filepath.Join(brewPrefix, "share", "bash-completion", "completions", "brick"),
				filepath.Join(brewPrefix, "share", "zsh", "site-functions", "_brick"),
			)
		}
	}
	var foundCompletions []string
	for _, f := range completionFiles {
		if _, err := os.Stat(f); err == nil {
			foundCompletions = append(foundCompletions, f)
		}
	}
	if len(foundCompletions) > 0 {
		fmt.Println("\nShell completion files found:")
		for _, f := range foundCompletions {
			fmt.Printf("  %s\n", f)
		}
		fmt.Print("Remove shell completion files? (y/N): ")
		resp, _ = reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp == "y" || resp == "yes" {
			for _, f := range foundCompletions {
				if err := os.Remove(f); err != nil {
					fmt.Printf("\033[31mFailed to remove %s: %v\033[0m\n", f, err)
				} else {
					fmt.Printf("\033[32mRemoved %s\033[0m\n", f)
				}
			}
		} else {
			fmt.Println("Skipped shell completion removal.")
		}
	}

	// --- Man page ---
	manPage := filepath.Join(home, ".local", "share", "man", "man1", "brick.1")
	if _, err := os.Stat(manPage); err == nil {
		fmt.Printf("\nMan page: %s\n", manPage)
		fmt.Print("Remove man page? (y/N): ")
		resp, _ = reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp == "y" || resp == "yes" {
			if err := os.Remove(manPage); err != nil {
				fmt.Printf("\033[31mFailed to remove man page: %v\033[0m\n", err)
			} else {
				fmt.Println("\033[32mMan page removed.\033[0m")
			}
		} else {
			fmt.Println("Skipped man page removal.")
		}
	}

	fmt.Println("\nUninstall complete.")
}

// runRestartCmd handles `brick restart`: clears local settings (config,
// optionally each account's local sync folder) and, unless the user backs
// out at the initial confirmation, immediately re-enters the same
// interactive foreground setup a plain `brick sync` would run — so restarting
// always leaves brick reconfigured and syncing, rather than requiring a
// separate `brick sync` call afterward. Mirrors the fallthrough behavior the
// old --restart flag got for free by sharing main()'s body with the default
// sync path.
func runRestartCmd(noUpgradeCheck, noControlAPI bool) {
	proceed, err := runRestart()
	if err != nil {
		log.Fatalf("Restart failed: %v", err)
	}
	if !proceed {
		return
	}

	if !noUpgradeCheck && !isRunningInDevelopment() {
		checkForUpdates()
	}
	apiURL := resolveAPIURL()
	storageURL := resolveStorageAPIURL()

	if err := runWithAutoRelogin(apiURL, authFailedReloginPrompt, func() error {
		return runStorageSync(apiURL, storageURL, false, noControlAPI)
	}); err != nil {
		if errors.Is(err, errLoginDeclined) {
			return
		}
		log.Fatalf("Storage sync failed: %v", err)
	}
}

// runRestart clears brick's local settings so it can be configured from
// scratch: it stops any running instance, offers to wipe each known
// account's sync folder, then removes ~/.config/brick entirely. The bool
// return is false if the user backed out at the initial confirmation, in
// which case the caller should stop rather than fall through into setup.
func runRestart() (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("This will clear existing settings and configure Brick from scratch. Continue (Y/n): ")
	resp, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("could not read input: %w", err)
	}
	resp = strings.TrimSpace(strings.ToLower(resp))
	if resp == "n" || resp == "no" {
		fmt.Println("Restart cancelled.")
		return false, nil
	}

	if _, err := stopRunningInstance("\nStopping the running brick instance..."); err != nil {
		return false, err
	}

	cfg, err := loadOrCreateConfig()
	if err != nil {
		return false, err
	}

	accountIDs := make([]string, 0, len(cfg.Accounts))
	for id := range cfg.Accounts {
		accountIDs = append(accountIDs, id)
	}
	sort.Strings(accountIDs)

	for _, id := range accountIDs {
		folder := cfg.Accounts[id].StorageSyncFolder
		if folder == "" {
			continue
		}
		if _, statErr := os.Stat(folder); os.IsNotExist(statErr) {
			continue
		}

		size, sizeErr := dirSize(folder)
		if sizeErr != nil {
			fmt.Printf("\033[31mCould not read %s: %v\033[0m\n", folder, sizeErr)
			continue
		}

		fmt.Printf("\nDo you want to remove the files in sync folder %s (%s) (y/N): ", folder, humanSize(size))
		resp, _ = reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			continue
		}
		if rmErr := removeDirContents(folder); rmErr != nil {
			fmt.Printf("\033[31mFailed to remove files in %s: %v\033[0m\n", folder, rmErr)
			continue
		}
		fmt.Println("\033[32mFiles removed.\033[0m")

		fmt.Printf("Do you want to remove the folder %s (y/N): ", folder)
		resp, _ = reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp == "y" || resp == "yes" {
			if rmErr := os.Remove(folder); rmErr != nil {
				fmt.Printf("\033[31mFailed to remove folder %s: %v\033[0m\n", folder, rmErr)
			} else {
				fmt.Println("\033[32mFolder removed.\033[0m")
			}
		}
	}

	cfgPath, err := configPath()
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(filepath.Dir(cfgPath)); err != nil {
		return false, fmt.Errorf("could not remove config directory: %w", err)
	}

	fmt.Println("\nSettings cleared. Setting up Brick from scratch...")
	return true, nil
}

// dirSize returns the total size in bytes of all regular files under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// removeDirContents removes every entry inside dir, leaving dir itself in place.
func removeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
