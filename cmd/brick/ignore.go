package main

import "strings"

// ignoredNames are OS metadata files and folders that are never synced in
// either direction. They change constantly (Finder rewrites .DS_Store whenever
// a window is opened or rearranged), mean nothing on another platform, and
// would otherwise churn uploads and conflicts between devices.
var ignoredNames = map[string]bool{
	// macOS
	".DS_Store":       true,
	".Spotlight-V100": true,
	".Trashes":        true,
	".fseventsd":      true,
	".TemporaryItems": true,
	"Icon\r":          true, // custom folder icon
	// Windows (case-insensitive filesystem, so matched case-insensitively)
	"thumbs.db":   true,
	"desktop.ini": true,
}

// isIgnoredName reports whether a single path component is never synced: an
// OS metadata file (see ignoredNames), an AppleDouble "._*" resource-fork
// file, or an in-progress download (tmpSuffix).
func isIgnoredName(name string) bool {
	if ignoredNames[name] || ignoredNames[strings.ToLower(name)] {
		return true
	}
	return strings.HasPrefix(name, "._") || strings.HasSuffix(name, tmpSuffix)
}

// isIgnoredPath reports whether any component of the slash-separated rel is
// ignored, so everything inside an ignored folder is ignored with it.
//
// Ignored paths are filtered out where both trees are built (local walk,
// watcher, remote walk, remote delta), so the file pass never sees them. A
// sync-state entry left over from before a name was ignored then has neither
// side and is simply dropped (reconcileFile's default case): nothing is
// trashed remotely or deleted locally.
func isIgnoredPath(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if isIgnoredName(part) {
			return true
		}
	}
	return false
}
