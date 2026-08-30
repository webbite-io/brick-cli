package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

// selfTestCheck is one diagnostic performed by --self-test. Status is "ok"
// (the underlying condition is good — sync can proceed past this check),
// "fail" (it isn't), or "skipped" (a prerequisite check already failed, so
// this one couldn't meaningfully run).
type selfTestCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// selfTestOutput is the sole line of output brick prints for --self-test,
// meant to be parsed deterministically by a companion app deciding whether a
// subsequent 'brick sync -d' is expected to succeed. Status is always "ok" — the
// self-test ran to completion — since pass/fail of the underlying conditions
// is carried by Ready and by each check's own Status, never by the process
// exit code (--self-test always exits 0).
type selfTestOutput struct {
	Status  string          `json:"status"`
	Version string          `json:"version"`
	Ready   bool            `json:"ready"`
	Checks  []selfTestCheck `json:"checks"`
}

// uiMessage normalizes a check's message so it's fit to show directly in a
// UI: capitalized first letter and a trailing period. Several messages are
// (or wrap) raw Go error text, which is conventionally lowercase and
// unpunctuated — this is applied centrally here rather than at each call
// site so no message, including one built from err.Error(), is missed.
func uiMessage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	switch s[len(s)-1] {
	case '.', '!', '?':
	default:
		s += "."
	}
	return s
}

func selfTestOK(id, msg string) selfTestCheck {
	return selfTestCheck{ID: id, Status: "ok", Message: uiMessage(msg)}
}

func selfTestFail(id, code, msg string) selfTestCheck {
	return selfTestCheck{ID: id, Status: "fail", Code: code, Message: uiMessage(msg)}
}

func selfTestSkipped(id, code, msg string) selfTestCheck {
	return selfTestCheck{ID: id, Status: "skipped", Code: code, Message: uiMessage(msg)}
}

// runSelfTest performs each readiness check in turn, independently of
// whether earlier ones failed (except where a check genuinely cannot run
// without a prior one's result, e.g. storage_reachable needs a loaded config
// and a token — those are reported "skipped" rather than attempted). It never
// prompts, writes to the sync folder, or starts syncing; the only state it
// may change is a token refresh during the authentication check, exactly as
// an ordinary 'brick whoami' would do.
func runSelfTest(apiURL, storageURL string) selfTestOutput {
	var checks []selfTestCheck

	// 1. Is another instance already running?
	lockPath, err := instanceLockPath()
	if err != nil {
		checks = append(checks, selfTestFail("instance_lock", "lock_path_error", err.Error()))
	} else if lock, lockErr := acquireInstanceLock(lockPath); lockErr != nil {
		if errors.Is(lockErr, errInstanceLocked) {
			checks = append(checks, selfTestFail("instance_lock", "already_running", "another brick instance is already running for this user"))
		} else {
			checks = append(checks, selfTestFail("instance_lock", "lock_error", lockErr.Error()))
		}
	} else {
		lock.Release()
		checks = append(checks, selfTestOK("instance_lock", "no other brick instance is running"))
	}

	// 2. Am I fully configured to sync?
	configOK := false
	cfg, _, err := loadOrCreateConfigQuiet()
	if err != nil {
		checks = append(checks, selfTestFail("configuration", "config_error", err.Error()))
	} else if cfg.ActiveAccountID == "" {
		checks = append(checks, selfTestFail("configuration", "no_active_account", "no account selected; run 'brick switch-accounts'"))
	} else if ac := cfg.activeAccount(); ac == nil || ac.StorageSyncFolder == "" {
		checks = append(checks, selfTestFail("configuration", "no_sync_folder", "no sync folder configured for the active account; run 'brick sync' interactively to finish setup"))
	} else if _, statErr := os.Stat(ac.StorageSyncFolder); statErr != nil {
		checks = append(checks, selfTestFail("configuration", "sync_folder_missing", fmt.Sprintf("configured sync folder %s is not accessible: %v", ac.StorageSyncFolder, statErr)))
	} else {
		configOK = true
		checks = append(checks, selfTestOK("configuration", fmt.Sprintf("syncing %s for account %s", ac.StorageSyncFolder, cfg.ActiveAccountID)))
	}

	// 3. Am I authenticated? A round trip to /oauth2/userinfo, same as
	// 'brick whoami' — authedGet silently refreshes the access token first
	// if needed, so this also confirms a stored refresh token still works.
	authOK := false
	if cfg == nil || (cfg.AccessToken == "" && cfg.RefreshToken == "") {
		checks = append(checks, selfTestFail("authentication", "not_logged_in", "not logged in; run 'brick login'"))
	} else {
		resp, reqErr := authedGet(apiURL, "/oauth2/userinfo", cfg.AccessToken, cfg)
		if reqErr != nil {
			if errors.Is(reqErr, errSessionExpired) {
				checks = append(checks, selfTestFail("authentication", "session_expired", "session has expired; run 'brick login' to re-authenticate"))
			} else {
				checks = append(checks, selfTestFail("authentication", "request_failed", reqErr.Error()))
			}
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				authOK = true
				checks = append(checks, selfTestOK("authentication", "access token is valid"))
			} else {
				checks = append(checks, selfTestFail("authentication", "unexpected_status", fmt.Sprintf("userinfo endpoint returned status %d", resp.StatusCode)))
			}
		}
	}

	// 4. Can I reach the Brick API? Independent of login state — the OIDC
	// discovery document is unauthenticated, so this isolates "no network /
	// API down" from "network is fine but my credentials aren't".
	if _, err := fetchOIDCConfig(apiURL); err != nil {
		checks = append(checks, selfTestFail("api_reachable", "unreachable", err.Error()))
	} else {
		checks = append(checks, selfTestOK("api_reachable", fmt.Sprintf("reached %s", apiURL)))
	}

	// 5. Can I reach the Storage API and resolve the account's root folder?
	// The most direct end-to-end check of "can I actually sync right now" —
	// the same call prepareSync makes before a real sync starts — but it
	// needs a configured account and a working token, so it's skipped rather
	// than attempted when either of those already failed above.
	switch {
	case !configOK:
		checks = append(checks, selfTestSkipped("storage_reachable", "not_configured", "skipped: brick is not fully configured"))
	case !authOK:
		checks = append(checks, selfTestSkipped("storage_reachable", "not_authenticated", "skipped: not authenticated"))
	default:
		sc := &storageClient{baseURL: storageURL, apiURL: apiURL, accountID: cfg.ActiveAccountID, cfg: cfg}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := sc.resolveRoot(ctx); err != nil {
			checks = append(checks, selfTestFail("storage_reachable", "unreachable", err.Error()))
		} else {
			checks = append(checks, selfTestOK("storage_reachable", fmt.Sprintf("reached %s", storageURL)))
		}
		cancel()
	}

	ready := true
	for _, c := range checks {
		if c.Status != "ok" {
			ready = false
			break
		}
	}

	return selfTestOutput{
		Status:  "ok",
		Version: Version,
		Ready:   ready,
		Checks:  checks,
	}
}

// emitSelfTestOutput prints out as the single line of JSON on stdout for
// --self-test and always exits 0. Pass/fail of the underlying checks is
// carried entirely by the JSON (Ready and each check's Status), never by the
// process exit code, so a companion app never has to special-case a "failed"
// self-test as a crash.
func emitSelfTestOutput(out selfTestOutput) {
	data, err := json.Marshal(out)
	if err != nil {
		// Should be unreachable (selfTestOutput only has marshalable
		// fields), but a companion app parsing stdout must never see
		// anything other than a single valid JSON line.
		data, _ = json.Marshal(selfTestOutput{
			Status: "error",
			Checks: []selfTestCheck{selfTestFail("internal", "marshal_error", err.Error())},
		})
	}
	fmt.Println(string(data))
	os.Exit(0)
}
