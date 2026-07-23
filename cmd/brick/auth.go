package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/google/uuid"
)

// errSessionExpired signals that a request failed because both the access
// token and the refresh token are no longer valid, so the caller must
// re-authenticate interactively.
var errSessionExpired = errors.New("session expired")

// oidcConfig holds the subset of fields from a .well-known/openid-configuration document.
type oidcConfig struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// fetchOIDCConfig retrieves the OIDC discovery document from apiURL/.well-known/openid-configuration.
func fetchOIDCConfig(apiURL string) (*oidcConfig, error) {
	discoveryURL := strings.TrimRight(apiURL, "/") + "/.well-known/openid-configuration"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", discoveryURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach OIDC discovery endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery returned status %d", resp.StatusCode)
	}

	var cfg oidcConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("could not parse OIDC discovery document: %w", err)
	}
	if cfg.AuthorizationEndpoint == "" || cfg.TokenEndpoint == "" {
		return nil, errors.New("OIDC discovery document is missing required endpoints")
	}
	return &cfg, nil
}

// generatePKCE returns a code_verifier and its S256 code_challenge.
func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("could not generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// openBrowser attempts to open url in the system default browser.
// Errors are ignored — the URL is always printed separately.
func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	_ = cmd.Start()
}

// exchangeCodeForToken exchanges an authorization code for an access token and optional refresh token.
func exchangeCodeForToken(tokenEndpoint, code, codeVerifier, redirectURI, clientID string) (accessToken, refreshToken, idToken string, err error) {
	params := url.Values{}
	params.Set("grant_type", "authorization_code")
	params.Set("code", code)
	params.Set("redirect_uri", redirectURI)
	params.Set("client_id", clientID)
	params.Set("code_verifier", codeVerifier)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", "", "", fmt.Errorf("could not parse token response: %w", err)
	}
	if tokenResp.Error != "" {
		return "", "", "", fmt.Errorf("token error %q: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if tokenResp.AccessToken == "" {
		return "", "", "", errors.New("token response did not contain an access_token")
	}
	return tokenResp.AccessToken, tokenResp.RefreshToken, tokenResp.IDToken, nil
}

// runWithAutoRelogin runs fn. If it returns an errSessionExpired error, it
// prompts the user to log in again and retries fn once after a successful login.
func runWithAutoRelogin(apiURL string, fn func() error) error {
	err := fn()
	if err == nil || !errors.Is(err, errSessionExpired) {
		return err
	}
	fmt.Print("\nYou've been logged out and need to login again, continue? (Y/n): ")
	reader := bufio.NewReader(os.Stdin)
	response, readErr := reader.ReadString('\n')
	if readErr != nil || strings.EqualFold(strings.TrimSpace(response), "n") {
		return errors.New("login cancelled")
	}
	if loginErr := runLogin(apiURL); loginErr != nil {
		return fmt.Errorf("login failed: %w", loginErr)
	}
	return fn()
}

// runLogin performs the OIDC Authorization Code + PKCE login flow.
func runLogin(apiURL string) error {
	clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
	if clientID == "" {
		return errors.New("OAUTH_CLIENT_ID is not set")
	}
	scopes := getEnv("OAUTH_SCOPES", DefaultOAuthScopes)
	callbackURL := getEnv("OAUTH_CALLBACK_URL", DefaultOAuthCallbackURL)

	// Parse callback URL to determine where to listen.
	parsedCB, err := url.Parse(callbackURL)
	if err != nil {
		return fmt.Errorf("invalid OAUTH_CALLBACK_URL: %w", err)
	}
	listenAddr := parsedCB.Host
	callbackPath := parsedCB.Path

	// Discover OIDC endpoints.
	oidc, err := fetchOIDCConfig(apiURL)
	if err != nil {
		return err
	}

	// Generate PKCE and state.
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return err
	}
	state := uuid.New().String()

	// Build the authorization URL.
	authParams := url.Values{}
	authParams.Set("response_type", "code")
	authParams.Set("client_id", clientID)
	authParams.Set("redirect_uri", callbackURL)
	authParams.Set("scope", scopes)
	authParams.Set("state", state)
	authParams.Set("code_challenge", challenge)
	authParams.Set("code_challenge_method", "S256")
	authURL := oidc.AuthorizationEndpoint + "?" + authParams.Encode()

	// Start local callback server before opening the browser.
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("state mismatch in callback")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			if e := r.URL.Query().Get("error"); e != "" {
				desc := r.URL.Query().Get("error_description")
				http.Error(w, "authorization denied", http.StatusBadRequest)
				errCh <- fmt.Errorf("authorization error %q: %s", e, desc)
				return
			}
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			errCh <- errors.New("callback did not contain an authorization code")
			return
		}
		fmt.Fprintln(w, "Login successful. You may close this tab.")
		codeCh <- code
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("callback server error: %w", err)
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	fmt.Printf("Opening browser for login...\n\n")
	fmt.Printf("If the browser does not open, visit this URL manually:\n\n  %s\n\n", authURL)
	openBrowser(authURL)
	fmt.Println("Waiting for authorization...")

	// Wait up to 5 minutes for the callback.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var code string
	select {
	case <-ctx.Done():
		return errors.New("login timed out waiting for browser callback")
	case err := <-errCh:
		return err
	case code = <-codeCh:
	}

	// Exchange code for token.
	accessToken, refreshToken, idToken, err := exchangeCodeForToken(oidc.TokenEndpoint, code, verifier, callbackURL, clientID)
	if err != nil {
		return err
	}

	// Persist tokens in config.
	cfg, err := loadOrCreateConfig()
	if err != nil {
		return err
	}
	cfg.AccessToken = accessToken
	cfg.RefreshToken = refreshToken
	cfg.IDToken = idToken

	// Fetch user info.
	userInfo, err := fetchUserInfo(apiURL, accessToken)
	if err != nil {
		return err
	}

	if userInfo.GivenName != "" && userInfo.FamilyName != "" {
		fmt.Printf("\nHello %s %s 👋\n", userInfo.GivenName, userInfo.FamilyName)
	} else {
		fmt.Println("\nLogin successful 🎉.")
	}

	// Fetch accounts and pick one (prompting if there's more than one).
	accounts, err := fetchAccounts(apiURL, accessToken)
	if err != nil {
		return err
	}
	accountID, accountName, err := selectAccount(accounts)
	if err != nil {
		return err
	}
	cfg.ActiveAccountID = accountID

	if err := saveConfig(cfg); err != nil {
		return err
	}

	fmt.Printf("Brick now has access to your account: %s.\n  To switch account, use the --switch-accounts parameter.", accountName)
	return nil
}

type userInfoResponse struct {
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

func fetchUserInfo(apiURL, accessToken string) (*userInfoResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(apiURL, "/")+"/oauth2/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned status %d", resp.StatusCode)
	}

	var info userInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("could not parse userinfo response: %w", err)
	}
	return &info, nil
}

type accountInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// fetchAccounts returns every account the authenticated user has access to.
func fetchAccounts(apiURL, accessToken string) ([]accountInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(apiURL, "/")+"/v1/accounts", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("accounts request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accounts endpoint returned status %d", resp.StatusCode)
	}

	var body struct {
		Accounts []accountInfo `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("could not parse accounts response: %w", err)
	}
	if len(body.Accounts) == 0 {
		return nil, errors.New("no accounts found for this user")
	}
	return body.Accounts, nil
}

// selectAccount returns the account to use: the only one if there's just one,
// otherwise an interactive prompt to pick from the list.
func selectAccount(accounts []accountInfo) (id, name string, err error) {
	if len(accounts) == 1 {
		return accounts[0].ID, accounts[0].Name, nil
	}

	fmt.Println("\nYou have access to more than one account, which one do you want to use?")
	fmt.Println()

	options := make([]huh.Option[string], len(accounts))
	for i, a := range accounts {
		options[i] = huh.NewOption(a.Name, a.ID)
	}
	var selected string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select an account").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return "", "", err
	}
	for _, a := range accounts {
		if a.ID == selected {
			return a.ID, a.Name, nil
		}
	}
	return "", "", errors.New("no account selected")
}

// refreshAccessToken uses a refresh token to obtain a new access token (and
// possibly a new refresh token and ID token).
func refreshAccessToken(apiURL, refreshToken, clientID string) (accessToken, newRefreshToken, idToken string, err error) {
	params := url.Values{}
	params.Set("grant_type", "refresh_token")
	params.Set("refresh_token", refreshToken)
	params.Set("client_id", clientID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(apiURL, "/")+"/oauth2/token", strings.NewReader(params.Encode()))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("token refresh returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", "", "", fmt.Errorf("could not parse token refresh response: %w", err)
	}
	if tokenResp.Error != "" {
		return "", "", "", fmt.Errorf("token refresh error %q: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if tokenResp.AccessToken == "" {
		return "", "", "", errors.New("token refresh response did not contain an access_token")
	}
	return tokenResp.AccessToken, tokenResp.RefreshToken, tokenResp.IDToken, nil
}

// errLoginDeclined signals that the user was prompted to log in on first run
// and said no — a deliberate exit, not a failure, so callers should terminate
// quietly instead of reporting an error.
var errLoginDeclined = errors.New("login declined")

// ensureAuthenticated ensures the config has a valid access token, refreshing or
// prompting login as needed. Returns the (possibly updated) config.
func ensureAuthenticated(apiURL string) (*Config, error) {
	cfg, err := loadOrCreateConfig()
	if err != nil {
		return nil, err
	}

	// No credentials at all — ask user to log in.
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		fmt.Print("Do you want to log in to get started? (Y/n): ")
		reader := bufio.NewReader(os.Stdin)
		response, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(strings.ToLower(response)) == "n" {
			return nil, errLoginDeclined
		}
		if err := runLogin(apiURL); err != nil {
			return nil, err
		}
		// Reload config after login.
		return loadOrCreateConfig()
	}

	// Have a refresh token but no access token — refresh silently.
	if cfg.AccessToken == "" && cfg.RefreshToken != "" {
		clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
		newAccess, newRefresh, newID, err := refreshAccessToken(apiURL, cfg.RefreshToken, clientID)
		if err != nil {
			return nil, fmt.Errorf("could not refresh credentials: %w", err)
		}
		cfg.AccessToken = newAccess
		if newRefresh != "" {
			cfg.RefreshToken = newRefresh
		}
		if newID != "" {
			cfg.IDToken = newID
		}
		if err := saveConfig(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	return cfg, nil
}

// authedGet performs a GET request with a Bearer token, retrying once after a
// token refresh if the server returns 401.
func authedGet(apiURL, path, accessToken string, cfg *Config) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	doRequest := func(token string) (*http.Response, error) {
		req, err := http.NewRequest("GET", strings.TrimRight(apiURL, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return client.Do(req)
	}

	resp, err := doRequest(accessToken)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && cfg.RefreshToken != "" {
		resp.Body.Close()
		clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
		newAccess, newRefresh, newID, refreshErr := refreshAccessToken(apiURL, cfg.RefreshToken, clientID)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; token refresh failed: %v", errSessionExpired, refreshErr)
		}
		cfg.AccessToken = newAccess
		if newRefresh != "" {
			cfg.RefreshToken = newRefresh
		}
		if newID != "" {
			cfg.IDToken = newID
		}
		if saveErr := saveConfig(cfg); saveErr != nil {
			return nil, saveErr
		}
		return doRequest(newAccess)
	}

	return resp, nil
}

// authedPost performs a POST request with a Bearer token, retrying once after a
// token refresh if the server returns 401.
func authedPost(apiURL, path, accessToken string, body []byte, cfg *Config) (*http.Response, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	doRequest := func(token string) (*http.Response, error) {
		req, err := http.NewRequest("POST", strings.TrimRight(apiURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	resp, err := doRequest(accessToken)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && cfg.RefreshToken != "" {
		resp.Body.Close()
		clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
		newAccess, newRefresh, newID, refreshErr := refreshAccessToken(apiURL, cfg.RefreshToken, clientID)
		if refreshErr != nil {
			return nil, fmt.Errorf("%w; token refresh failed: %v", errSessionExpired, refreshErr)
		}
		cfg.AccessToken = newAccess
		if newRefresh != "" {
			cfg.RefreshToken = newRefresh
		}
		if newID != "" {
			cfg.IDToken = newID
		}
		if saveErr := saveConfig(cfg); saveErr != nil {
			return nil, saveErr
		}
		return doRequest(newAccess)
	}

	return resp, nil
}

// runSwitchAccounts lists the user's accounts and lets them pick one to store
// as active. If the picked account has never been synced before, it runs the
// same sync-folder/scope onboarding as a brand-new setup. Finally, if a brick
// daemon is currently running, it's stopped and (if it was a background
// daemon) relaunched so it picks up the new account without a manual restart.
func runSwitchAccounts(apiURL, storageURL string) error {
	cfg, err := ensureAuthenticated(apiURL)
	if err != nil {
		return err
	}

	resp, err := authedGet(apiURL, "/v1/accounts", cfg.AccessToken, cfg)
	if err != nil {
		return fmt.Errorf("accounts request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("accounts endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("could not parse accounts response: %w", err)
	}
	if len(payload.Accounts) == 0 {
		return errors.New("no accounts found for this user")
	}

	fmt.Println("Pick one of your accounts:")
	fmt.Println()
	for i, a := range payload.Accounts {
		fmt.Printf("  %d. %s\n", i+1, a.Name)
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Enter number (1-%d): ", len(payload.Accounts))
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("could not read input: %w", err)
		}
		line = strings.TrimSpace(line)
		var choice int
		if _, err := fmt.Sscanf(line, "%d", &choice); err != nil || choice < 1 || choice > len(payload.Accounts) {
			fmt.Printf("Please enter a number between 1 and %d.\n", len(payload.Accounts))
			continue
		}
		selected := payload.Accounts[choice-1]
		isNewAccount := cfg.Accounts[selected.ID] == nil
		cfg.ActiveAccountID = selected.ID
		if err := saveConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("Active account set to: %s\n", selected.Name)

		if isNewAccount {
			if err := onboardNewAccount(apiURL, storageURL, cfg); err != nil {
				return err
			}
		}

		return restartDaemonIfRunning(apiURL, storageURL)
	}
}

// onboardNewAccount runs the same sync-folder and sync-scope onboarding a
// brand-new setup goes through, for an account picked via --switch-accounts
// that has never been synced on this device before.
func onboardNewAccount(apiURL, storageURL string, cfg *Config) error {
	fmt.Println("\nThis account hasn't been synced on this device before — let's set it up.")

	folder, _, err := ensureStorageSyncFolder(cfg)
	if err != nil {
		return err
	}

	sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}
	root, err := sc.resolveRoot(context.Background())
	if err != nil {
		return fmt.Errorf("could not reach storage API at %s: %w", storageURL, err)
	}

	if err := runSyncScopeOnboarding(sc, root.ID, cfg); err != nil {
		return err
	}

	fmt.Printf("Now, we're ready. Brick will sync %s the next time it runs.\n", folder)
	return nil
}

// runWhoami prints the authenticated user's name, email, and active account.
func runWhoami(apiURL string) error {
	cfg, err := loadOrCreateConfig()
	if err != nil {
		return err
	}
	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		fmt.Println("You are not logged in. Run 'brick --login' to authenticate.")
		return nil
	}

	// Silently refresh if we only have a refresh token.
	if cfg.AccessToken == "" {
		clientID := getEnv("OAUTH_CLIENT_ID", DefaultOAuthClientID)
		newAccess, newRefresh, newID, refreshErr := refreshAccessToken(apiURL, cfg.RefreshToken, clientID)
		if refreshErr != nil {
			fmt.Println("Your session has expired. Run 'brick --login' to re-authenticate.")
			return nil
		}
		cfg.AccessToken = newAccess
		if newRefresh != "" {
			cfg.RefreshToken = newRefresh
		}
		if newID != "" {
			cfg.IDToken = newID
		}
		_ = saveConfig(cfg)
	}

	// Fetch user info.
	profileResp, err := authedGet(apiURL, "/oauth2/userinfo", cfg.AccessToken, cfg)
	if err != nil {
		if errors.Is(err, errSessionExpired) {
			fmt.Println("Your session has expired. Run 'brick --login' to re-authenticate.")
			return nil
		}
		return fmt.Errorf("userinfo request failed: %w", err)
	}
	defer profileResp.Body.Close()

	if profileResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(profileResp.Body)
		return fmt.Errorf("userinfo endpoint returned status %d: %s", profileResp.StatusCode, string(body))
	}

	var profile struct {
		Email      string `json:"email"`
		GivenName  string `json:"given_name"`
		FamilyName string `json:"family_name"`
	}
	if err := json.NewDecoder(profileResp.Body).Decode(&profile); err != nil {
		return fmt.Errorf("could not parse userinfo response: %w", err)
	}

	// Fetch account name if an account is selected.
	var accountName string
	if cfg.ActiveAccountID != "" {
		acctResp, acctErr := authedGet(apiURL, "/v1/accounts/"+cfg.ActiveAccountID, cfg.AccessToken, cfg)
		if acctErr == nil {
			defer acctResp.Body.Close()
			if acctResp.StatusCode == http.StatusOK {
				var acctPayload struct {
					Account struct {
						Name string `json:"name"`
					} `json:"account"`
				}
				if jsonErr := json.NewDecoder(acctResp.Body).Decode(&acctPayload); jsonErr == nil {
					accountName = acctPayload.Account.Name
				}
			}
		}
	}

	fmt.Println("You're logged in as:")
	fmt.Println()
	name := strings.TrimSpace(profile.GivenName + " " + profile.FamilyName)
	if name != "" {
		fmt.Printf("- Name:    %s\n", name)
	}
	if profile.Email != "" {
		fmt.Printf("- Email:   %s\n", profile.Email)
	}
	if accountName != "" {
		fmt.Printf("- Account: %s\n", accountName)
	}
	return nil
}
