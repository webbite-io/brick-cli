package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Version information — set via ldflags at build time.
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"

	DefaultAPIURL           = ""
	DefaultStorageAPIURL    = ""
	DefaultOAuthClientID    = ""
	DefaultOAuthScopes      = "openid email profile accounts offline_access brick:manage"
	DefaultOAuthCallbackURL = "http://localhost:7332/auth/callback"
)

// Environment variables daemon mode (-d) uses to hand the outcome of the
// interactive setup (sync folder, first-run conflict choice) from the
// foreground process to the detached child it re-execs itself as. The
// child's presence check is daemonFolderEnv being non-empty.
const (
	daemonFolderEnv       = "BRICK_DAEMON_FOLDER"
	daemonConflictModeEnv = "BRICK_DAEMON_CONFLICT_MODE"
	daemonFirstSetupEnv   = "BRICK_DAEMON_FIRST_SETUP"
)

type Config struct {
	ClientID     string `yaml:"clientId"`
	AccessToken  string `yaml:"accessToken,omitempty"`
	RefreshToken string `yaml:"refreshToken,omitempty"`
	IDToken      string `yaml:"idToken,omitempty"`

	// ActiveAccountID is the account currently in effect; it always keys into
	// Accounts. Switched via 'brick --switch-accounts'.
	ActiveAccountID string `yaml:"activeAccountId,omitempty"`

	// Accounts holds one entry per account the user has ever synced, keyed by
	// account ID, so each remembers its own sync folder/scope independently of
	// which account is currently active.
	Accounts map[string]*AccountConfig `yaml:"accounts,omitempty"`

	// Remote file agent: additional directories that -r/--remote-control
	// exposes to remote clients attached via the storage API. Global rather
	// than per-account: which extra directories a device shares isn't tied to
	// any one Brick account.
	AgentRoots []string `yaml:"agentRoots,omitempty"`

	// RemoteControl, when true, enables remote file access (equivalent to
	// always passing -r/--remote-control) without needing the flag on every
	// run. Set during first-time onboarding if the user opts in; -r still
	// forces it on for a single invocation if the user declined here.
	RemoteControl bool `yaml:"remoteControl,omitempty"`
}

// AccountConfig holds the per-account settings for one entry in Config.Accounts.
type AccountConfig struct {
	StorageSyncFolder string `yaml:"storageSyncFolder,omitempty"`

	// ExcludeDirs lists folder paths, relative to StorageSyncFolder and
	// slash-separated (e.g. "folder/subfolder"), whose files are never
	// uploaded or downloaded. Changes under them are still detected and
	// logged, just not synced.
	ExcludeDirs []string `yaml:"excludeDirs,omitempty"`
}

// activeAccount returns the AccountConfig for cfg.ActiveAccountID, or nil if
// there is no active account or no entry for it yet.
func (c *Config) activeAccount() *AccountConfig {
	if c.ActiveAccountID == "" {
		return nil
	}
	return c.Accounts[c.ActiveAccountID]
}

// ensureActiveAccount returns the AccountConfig for cfg.ActiveAccountID,
// creating an empty entry (and the Accounts map itself, if needed) when one
// doesn't exist yet. Panics if ActiveAccountID is unset, since callers are
// expected to have one selected (e.g. via --switch-accounts) beforehand.
func (c *Config) ensureActiveAccount() *AccountConfig {
	if c.ActiveAccountID == "" {
		panic("ensureActiveAccount called with no active account set")
	}
	if c.Accounts == nil {
		c.Accounts = map[string]*AccountConfig{}
	}
	ac, ok := c.Accounts[c.ActiveAccountID]
	if !ok {
		ac = &AccountConfig{}
		c.Accounts[c.ActiveAccountID] = ac
	}
	return ac
}

// configPath returns the absolute path to ~/.config/brick/config.yaml.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "brick", "config.yaml"), nil
}

// saveConfig persists cfg to the config file with mode 0600.
func saveConfig(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("could not marshal config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("could not write config file: %w", err)
	}
	// Ensure permissions are 0600 even if the file already existed with looser perms.
	return os.Chmod(path, 0o600)
}

// loadOrCreateConfig reads ~/.config/brick/config.yaml, creating it with a
// fresh UUIDv4 clientId if it does not already exist.
func loadOrCreateConfig() (*Config, error) {
	cfg, created, err := loadOrCreateConfigQuiet()
	if err != nil {
		return nil, err
	}
	if created {
		fmt.Println("\n👋 Hello and welcome to Brick - storage for all your devices!\n")
		fmt.Println("Created default configuration file in ~/.config/brick/config.yaml\n")
	}
	return cfg, nil
}

// loadOrCreateConfigQuiet is loadOrCreateConfig without the "file created"
// print, for callers (like -d --json) that must never write anything to
// stdout beyond their own single-line output.
func loadOrCreateConfigQuiet() (cfg *Config, created bool, err error) {
	cfgPath, err := configPath()
	if err != nil {
		return nil, false, err
	}
	cfgDir := filepath.Dir(cfgPath)

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("could not read config file: %w", err)
	}

	var c Config
	if os.IsNotExist(err) {
		// Create directory and file with a new clientId.
		if mkErr := os.MkdirAll(cfgDir, 0o755); mkErr != nil {
			return nil, false, fmt.Errorf("could not create config directory: %w", mkErr)
		}
		c.ClientID = uuid.New().String()
		if saveErr := saveConfig(&c); saveErr != nil {
			return nil, false, saveErr
		}
		return &c, true, nil
	}

	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, false, fmt.Errorf("could not parse config file: %w", err)
	}

	// Populate missing clientId and persist.
	if c.ClientID == "" {
		c.ClientID = uuid.New().String()
		if saveErr := saveConfig(&c); saveErr != nil {
			return nil, false, saveErr
		}
	}

	return &c, false, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// resolveAPIURL returns the effective ACC_API_URL, preferring (in order):
// the runtime env var, the compile-time default, then a localhost dev fallback.
func resolveAPIURL() string {
	if v := getEnv("ACC_API_URL", ""); v != "" {
		return v
	}
	if DefaultAPIURL != "" {
		return DefaultAPIURL
	}
	return "http://localhost:8080"
}

// resolveStorageAPIURL returns the effective Storage API base URL, preferring
// the runtime STORAGE_API_URL env var, then the compile-time default, then the
// local development fallback.
func resolveStorageAPIURL() string {
	if v := getEnv("STORAGE_API_URL", ""); v != "" {
		return v
	}
	if DefaultStorageAPIURL != "" {
		return DefaultStorageAPIURL
	}
	return "http://localhost:8081"
}

func printHelp() {
	fmt.Printf("\n\033[38;5;208mRequestBite Brick CLI\033[0m ⚡ v%s\n\n", Version)
	fmt.Println("Usage:")
	fmt.Printf("  brick [options]\n\n")
	fmt.Println("Options:")
	fmt.Println("\nAccount Mgmt\n============")
	fmt.Printf("      --login                 Log in via browser\n")
	fmt.Printf("      --switch-accounts       Switch the active account\n")
	fmt.Printf("      --whoami                Show logged-in user and account details\n")
	fmt.Printf("      --restart               Clear existing settings and configure Brick from scratch\n")
	fmt.Println("\nStorage Sync\n============")
	fmt.Printf("  Running brick with no other options syncs storageSyncFolder with the Storage API and watches for changes\n")
	fmt.Printf("  -d, --daemon                Detach into the background once logged in and the Storage API is reachable\n")
	fmt.Printf("  -r, --remote-control        Allow the Storage API to remotely list/browse/transfer files on this device (also enabled by default if remoteControl: true in config file)\n")
	fmt.Printf("      --agent-root PATH       Additional directory to expose to remote clients when remote control is enabled (repeatable)\n")
	fmt.Println("\nOther\n=====")
	fmt.Printf("      --no-upgrade-check      Disable automatic upgrade check\n")
	fmt.Printf("      --no-control-api        Disable the local status/control API (used by tray apps)\n")
	fmt.Printf("      --uninstall             Uninstall brick\n")
	fmt.Printf("  -h, --help                  Show help information\n")
	fmt.Printf("  -v, --version               Show version information\n")
}
