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
		showVersion       bool
		showHelp          bool
		noUpgradeCheck    bool
		uninstall         bool
		loginMode         bool
		switchAccounts    bool
		whoami            bool
		restart           bool
		remoteControl     bool
		noControlAPI      bool
		daemon            bool
		daemonJSON        bool
		selectiveSync     bool
		listSelectiveSync bool
	)

	flag.BoolVar(&showVersion, "v", false, "")
	flag.BoolVar(&showVersion, "version", false, "")
	flag.BoolVar(&showHelp, "h", false, "")
	flag.BoolVar(&showHelp, "help", false, "")
	flag.BoolVar(&noUpgradeCheck, "no-upgrade-check", false, "")
	flag.BoolVar(&uninstall, "uninstall", false, "")
	flag.BoolVar(&loginMode, "login", false, "")
	flag.BoolVar(&switchAccounts, "switch-accounts", false, "")
	flag.BoolVar(&whoami, "whoami", false, "")
	flag.BoolVar(&restart, "restart", false, "")
	flag.BoolVar(&remoteControl, "r", false, "")
	flag.BoolVar(&remoteControl, "remote-control", false, "")
	flag.BoolVar(&noControlAPI, "no-control-api", false, "")
	flag.BoolVar(&daemon, "d", false, "")
	flag.BoolVar(&daemon, "daemon", false, "")
	flag.BoolVar(&selectiveSync, "s", false, "")
	flag.BoolVar(&selectiveSync, "selective-sync", false, "")
	flag.BoolVar(&listSelectiveSync, "list-selective-sync", false, "")
	// Undocumented: only used together with -d/--daemon, by the companion app
	// that starts brick in daemon mode. See README for the JSON output shapes.
	flag.BoolVar(&daemonJSON, "json", false, "")
	flag.Var(&agentRootsFlag, "agent-root", "")
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

	// Uninstall flow
	if uninstall {
		runUninstall()
		os.Exit(0)
	}

	// Login flow
	if loginMode {
		apiURL := resolveAPIURL()
		if err := runLogin(apiURL); err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		os.Exit(0)
	}

	// Switch accounts
	if switchAccounts {
		apiURL := resolveAPIURL()
		storageURL := resolveStorageAPIURL()
		if err := runWithAutoRelogin(apiURL, func() error { return runSwitchAccounts(apiURL, storageURL) }); err != nil {
			log.Fatalf("Switch accounts failed: %v", err)
		}
		os.Exit(0)
	}

	// Whoami
	if whoami {
		apiURL := resolveAPIURL()
		if err := runWhoami(apiURL); err != nil {
			log.Fatalf("whoami failed: %v", err)
		}
		os.Exit(0)
	}

	// Selective sync: update which folders are excluded from sync.
	if selectiveSync {
		apiURL := resolveAPIURL()
		storageURL := resolveStorageAPIURL()
		if err := runWithAutoRelogin(apiURL, func() error { return runSelectiveSync(apiURL, storageURL) }); err != nil {
			log.Fatalf("Selective sync failed: %v", err)
		}
		os.Exit(0)
	}

	// List selective sync: print the folders currently excluded from sync.
	if listSelectiveSync {
		if err := runListSelectiveSync(); err != nil {
			log.Fatalf("List selective sync failed: %v", err)
		}
		os.Exit(0)
	}

	// Restart: wipe local settings and sync folders, then fall through into
	// the normal flow below so setup runs again as if this were a fresh
	// install.
	if restart {
		proceed, err := runRestart()
		if err != nil {
			log.Fatalf("Restart failed: %v", err)
		}
		if !proceed {
			os.Exit(0)
		}
	}

	// Storage sync: the default action, requiring no CLI options. Skipped
	// entirely under -d --json: checkForUpdates can print a prompt and read
	// stdin, which would break the "exactly one JSON line on stdout" contract.
	if !noUpgradeCheck && !(daemon && daemonJSON) && !isRunningInDevelopment() {
		checkForUpdates()
	}
	apiURL := resolveAPIURL()
	storageURL := resolveStorageAPIURL()

	// Detached daemon child: runAsDaemon re-execs the binary with this env var
	// set, handing over the folder/conflict-mode decisions made interactively
	// in the foreground parent so they're applied here without prompting again.
	if folder := os.Getenv(daemonFolderEnv); folder != "" {
		isFirstSetup := os.Getenv(daemonFirstSetupEnv) == "1"
		if err := runDaemonChild(apiURL, storageURL, remoteControl, noControlAPI, folder, os.Getenv(daemonConflictModeEnv), isFirstSetup); err != nil {
			if errors.Is(err, errLoginDeclined) {
				os.Exit(0)
			}
			log.Fatalf("Storage sync failed: %v", err)
		}
		os.Exit(0)
	}

	if daemon {
		if daemonJSON {
			runAsDaemonJSON(apiURL, storageURL, remoteControl, noControlAPI)
			return // unreachable: runAsDaemonJSON always exits the process itself
		}
		if err := runAsDaemon(apiURL, storageURL, remoteControl, noControlAPI); err != nil {
			if errors.Is(err, errLoginDeclined) {
				os.Exit(0)
			}
			log.Fatalf("Failed to start daemon: %v", err)
		}
		os.Exit(0)
	}

	if err := runStorageSync(apiURL, storageURL, remoteControl, noControlAPI); err != nil {
		if errors.Is(err, errLoginDeclined) {
			os.Exit(0)
		}
		log.Fatalf("Storage sync failed: %v", err)
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

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/requestbite/brick/releases/latest", nil)
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
		fmt.Printf("See https://github.com/requestbite/brick/ for installation details.\n\n")
		return
	}

	fmt.Print("Do you want to install (Y/N): ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("\nContinuing with current version...")
		return
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response == "y" || response == "yes" {
		fmt.Println("\nInstalling update...")
		if err := installUpdate(); err != nil {
			fmt.Printf("\033[31mFailed to install update: %v\033[0m\n", err)
			fmt.Printf("Please visit https://github.com/requestbite/brick/ for manual installation.\n\n")
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
	cmd := exec.Command("bash", "-c", "curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash")
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
