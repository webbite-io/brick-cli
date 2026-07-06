//go:build windows

package main

import "errors"

// Daemon mode relies on POSIX session detachment (setsid) and fd inheritance
// across exec, neither of which Windows has an equivalent for, so -d/--daemon
// is unsupported there.
var errDaemonUnsupported = errors.New("daemon mode (-d) is not supported on Windows; run brick without -d instead")

func runAsDaemon(apiURL, storageURL string, remoteControl, noControlAPI bool) error {
	return errDaemonUnsupported
}

func runDaemonChild(apiURL, storageURL string, remoteControl, noControlAPI bool, folder, conflictMode string, isFirstSetup bool) error {
	return errDaemonUnsupported
}

// runAsDaemonJSON is the --json counterpart to runAsDaemon; daemon mode is
// unsupported on Windows regardless, so it reports that as a JSON error and
// exits (via emitDaemonJSON) rather than returning.
func runAsDaemonJSON(apiURL, storageURL string, remoteControl, noControlAPI bool) {
	emitDaemonJSON(daemonJSONOutput{Status: "error", Code: "unsupported_platform", Message: errDaemonUnsupported.Error()})
}
