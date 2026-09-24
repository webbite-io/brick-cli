//go:build windows

package main

import "errors"

// Daemon mode relies on POSIX session detachment (setsid) and fd inheritance
// across exec, neither of which Windows has an equivalent for, so -d/--daemon
// is unsupported there.
var errDaemonUnsupported = errors.New("daemon mode (-d) is not supported on Windows; run 'brick sync' without -d instead")

// daemonSupported reports whether this platform can run brick as a detached
// background daemon; used to decide whether the interactive sync banner
// advertises the 'D' detach shortcut at all.
const daemonSupported = false

func runAsDaemon(apiURL, storageURL string, remoteControl bool) error {
	return errDaemonUnsupported
}

func runDaemonChild(apiURL, storageURL string, remoteControl bool, folder, conflictMode string, isFirstSetup bool) error {
	return errDaemonUnsupported
}

// runAsDaemonJSON is the --json counterpart to runAsDaemon; daemon mode is
// unsupported on Windows regardless, so it reports that as a JSON error and
// exits (via emitDaemonJSON) rather than returning.
func runAsDaemonJSON(apiURL, storageURL string, remoteControl bool) {
	emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "unsupported_platform", Message: errDaemonUnsupported.Error()})
}
