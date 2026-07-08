//go:build windows

package main

import "errors"

// Daemon mode relies on POSIX session detachment (setsid) and fd inheritance
// across exec, neither of which Windows has an equivalent for, so -d/--daemon
// is unsupported there.
var errDaemonUnsupported = errors.New("daemon mode (-d) is not supported on Windows; run brick without -d instead")

// daemonSupported reports whether this platform can run brick as a detached
// background daemon; used to decide whether the interactive sync banner
// advertises the 'D' detach shortcut at all.
const daemonSupported = false

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

// relaunchDaemon mirrors runAsDaemon's Windows unsupported-ness: since -d is
// never available here, a control discovery file can never report
// Background: true on this platform, so this should be unreachable in
// practice — but restartDaemonIfRunning is cross-platform code, so it needs
// a symbol to call.
func relaunchDaemon(apiURL, storageURL string, remoteControl bool, agentRoots []string) error {
	return errDaemonUnsupported
}
