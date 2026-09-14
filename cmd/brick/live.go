package main

import (
	"fmt"
)

const (
	ansiReset      = "\033[0m"
	ansiPurple     = "\033[38;5;135m"
	ansiLightGreen = "\033[38;5;120m"
	ansiYellow     = "\033[38;5;226m"
	ansiOrange     = "\033[38;5;208m"
	ansiRed        = "\033[38;5;196m"
)

// quotaWarnRatio and quotaCriticalRatio are the fractions of the account's
// storage quota at which the storage line turns orange and then red.
const (
	quotaWarnRatio     = 0.75
	quotaCriticalRatio = 0.95
)

// printSyncBanner prints the one-line banner shown before the sync loop
// starts in non-interactive mode (redirected output, or the detached daemon
// child). Interactive mode renders its own header — name, version, and sync
// folder — as the top row of the full-screen sync TUI instead (see tui.go).
func printSyncBanner(folder string) {
	fmt.Printf("I'm %sWebbite Brick CLI%s v%s, syncing %s with Brick. Press Ctrl+C to stop.\n", ansiYellow, ansiReset, Version, folder)
}

// quotaLine renders the banner's storage usage line, e.g.
//
//	Storage: 50.9 GB of 500.0 GB used total (your share 28.0 GB)
//
// The "X of Y used total" part turns orange once usage passes
// quotaWarnRatio of the quota and red past quotaCriticalRatio, so an account
// running out of room is visible at a glance. Returns "" when no quota has
// been fetched yet, or when the account reports no quota at all — there is no
// ratio to render against in that case.
func quotaLine(q *storageQuota) string {
	if q == nil || q.QuotaBytes <= 0 {
		return ""
	}
	used := fmt.Sprintf("%s of %s used total", humanSize(q.UsedBytes), humanSize(q.QuotaBytes))
	switch ratio := float64(q.UsedBytes) / float64(q.QuotaBytes); {
	case ratio > quotaCriticalRatio:
		used = ansiRed + used + ansiReset
	case ratio > quotaWarnRatio:
		used = ansiOrange + used + ansiReset
	}
	return fmt.Sprintf("%sStorage: %s %s (your share %s)", ansiPurple, ansiReset, used, humanSize(q.CallingUser.UsedBytes))
}

// visibleWidth counts the runes in line that occupy a terminal column,
// ignoring ANSI escape sequences. Used by the onboarding checklist to figure
// out how many rows a printed line will wrap to (see rowsForLine in
// sync.go).
func visibleWidth(line string) int {
	n, inEscape := 0, false
	for _, r := range line {
		switch {
		case inEscape:
			inEscape = r != 'm'
		case r == '\033':
			inEscape = true
		default:
			n++
		}
	}
	return n
}
