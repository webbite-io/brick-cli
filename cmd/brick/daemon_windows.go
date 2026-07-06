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
