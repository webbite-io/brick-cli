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

type Config struct {
	ClientID     string `yaml:"clientId"`
	AccessToken  string `yaml:"accessToken,omitempty"`
	RefreshToken string `yaml:"refreshToken,omitempty"`
	IDToken      string `yaml:"idToken,omitempty"`
	AccountID    string `yaml:"accountId,omitempty"`

	// Storage sync
	StorageSyncFolder string `yaml:"storageSyncFolder,omitempty"`

	// Remote file agent: additional directories (beyond the sync folder) that
	// -s/--sync exposes to remote clients attached via the storage API.
	AgentRoots []string `yaml:"agentRoots,omitempty"`
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
	cfgPath, err := configPath()
	if err != nil {
		return nil, err
	}
	cfgDir := filepath.Dir(cfgPath)

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not read config file: %w", err)
	}

	var cfg Config
	if os.IsNotExist(err) {
		// Create directory and file with a new clientId.
		if mkErr := os.MkdirAll(cfgDir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("could not create config directory: %w", mkErr)
		}
		cfg.ClientID = uuid.New().String()
		if saveErr := saveConfig(&cfg); saveErr != nil {
			return nil, saveErr
		}
		fmt.Printf("Created default configuration file in ~/.config/brick/config.yaml\n")
		return &cfg, nil
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("could not parse config file: %w", err)
	}

	// Populate missing clientId and persist.
	if cfg.ClientID == "" {
		cfg.ClientID = uuid.New().String()
		if saveErr := saveConfig(&cfg); saveErr != nil {
			return nil, saveErr
		}
	}

	return &cfg, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// resolveAPIURL returns the effective REQUESTBITE_API_URL, preferring (in order):
// the runtime env var, the compile-time default, then a localhost dev fallback.
func resolveAPIURL() string {
	if v := getEnv("REQUESTBITE_API_URL", ""); v != "" {
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
	fmt.Println("\nStorage Sync\n============")
	fmt.Printf("  -s, --sync                  Sync storageSyncFolder with the Storage API and watch for changes\n")
	fmt.Printf("      --agent-root PATH       Additional directory to expose to remote clients during sync (repeatable)\n")
	fmt.Println("\nOther\n=====")
	fmt.Printf("      --no-upgrade-check      Disable automatic upgrade check\n")
	fmt.Printf("      --uninstall             Uninstall brick\n")
	fmt.Printf("  -h, --help                  Show help information\n")
	fmt.Printf("  -v, --version               Show version information\n")
}
