package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsIgnoredPath(t *testing.T) {
	for rel, want := range map[string]bool{
		".DS_Store":                   true,
		"photos/.DS_Store":            true,
		"photos/._IMG_0001.jpg":       true,
		".Spotlight-V100/Store-V2/db": true,
		".Trashes":                    true,
		"docs/Icon\r":                 true,
		"Thumbs.db":                   true,
		"docs/THUMBS.DB":              true,
		"docs/Desktop.ini":            true,
		"partial.bin" + tmpSuffix:     true,
		"a.txt":                       false,
		".DS_Store.txt":               false,
		"docs/Icon":                   false,
		"my.DS_Store/a.txt":           false,
		".env":                        false,
		"_notes/a.txt":                false,
	} {
		if got := isIgnoredPath(rel); got != want {
			t.Errorf("isIgnoredPath(%q) = %v, want %v", rel, got, want)
		}
	}
}

// fakeRemoteNames returns the names of every live node in the fake server.
func fakeRemoteNames(fs *fakeStorageAPI) map[string]bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	names := map[string]bool{}
	for _, n := range fs.nodes {
		if !n.IsDeleted {
			names[n.Name] = true
		}
	}
	return names
}

func writeLocalAll(t *testing.T, e *syncEngine, rel, content string) {
	t.Helper()
	abs := filepath.Join(e.folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOSMetadataFilesAreNeverUploaded(t *testing.T) {
	eng, fs := newLocalTreeCacheTestEngine(t)
	writeLocalAll(t, eng, "a.txt", "A")
	writeLocalAll(t, eng, ".DS_Store", "finder")
	writeLocalAll(t, eng, "photos/.DS_Store", "finder")
	writeLocalAll(t, eng, "photos/._p.jpg", "fork")
	writeLocalAll(t, eng, "photos/Thumbs.db", "thumbs")
	writeLocalAll(t, eng, ".Trashes/501/old.txt", "trash")

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	names := fakeRemoteNames(fs)
	if !names["a.txt"] || !names["photos"] {
		t.Errorf("a.txt and photos/ not uploaded: %v", names)
	}
	for _, name := range []string{".DS_Store", "._p.jpg", "Thumbs.db", ".Trashes", "old.txt"} {
		if names[name] {
			t.Errorf("%s uploaded", name)
		}
	}
	for rel := range eng.state.Entries {
		if isIgnoredPath(rel) {
			t.Errorf("%s recorded in the sync index", rel)
		}
	}
}

func TestOSMetadataFilesAreNeverDownloaded(t *testing.T) {
	eng, fs := newLocalTreeCacheTestEngine(t)
	fs.mu.Lock()
	fs.nodes["f1"] = &fakeNode{storageNode: storageNode{ID: "f1", ParentID: "root", Name: "a.txt", NodeType: "file", SizeBytes: 1}, data: []byte("A")}
	fs.nodes["f2"] = &fakeNode{storageNode: storageNode{ID: "f2", ParentID: "root", Name: ".DS_Store", NodeType: "file", SizeBytes: 1}, data: []byte("D")}
	fs.nodes["f3"] = &fakeNode{storageNode: storageNode{ID: "f3", ParentID: "root", Name: "desktop.ini", NodeType: "file", SizeBytes: 1}, data: []byte("I")}
	fs.nodes["d1"] = &fakeNode{storageNode: storageNode{ID: "d1", ParentID: "root", Name: ".Spotlight-V100", NodeType: "folder"}}
	fs.nodes["f4"] = &fakeNode{storageNode: storageNode{ID: "f4", ParentID: "d1", Name: "db", NodeType: "file", SizeBytes: 1}, data: []byte("S")}
	fs.mu.Unlock()

	ctx := context.Background()
	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(eng.folder, "a.txt")); err != nil {
		t.Errorf("a.txt not downloaded: %v", err)
	}
	for _, rel := range []string{".DS_Store", "desktop.ini", ".Spotlight-V100"} {
		if _, err := os.Stat(filepath.Join(eng.folder, rel)); err == nil {
			t.Errorf("%s downloaded", rel)
		}
	}

	// And their absence locally is never mistaken for a local delete.
	if err := eng.forceReconcile(ctx); err != nil {
		t.Fatalf("forceReconcile: %v", err)
	}
	names := fakeRemoteNames(fs)
	for _, name := range []string{".DS_Store", "desktop.ini", ".Spotlight-V100", "db"} {
		if !names[name] {
			t.Errorf("remote %s was trashed", name)
		}
	}
}

// The watcher reports .DS_Store writes like any other; they must not reach the
// file pass, including inside a directory reported as a whole.
func TestWatcherReportedOSMetadataIsSkipped(t *testing.T) {
	eng, fs := newLocalTreeCacheTestEngine(t)
	ctx := context.Background()
	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("bootstrap reconcileAll: %v", err)
	}
	writeLocalAll(t, eng, ".DS_Store", "finder")
	writeLocalAll(t, eng, "imported/.DS_Store", "finder")
	writeLocalAll(t, eng, "imported/a.txt", "A")

	if err := eng.reconcileLocalChanges(ctx, []string{".DS_Store", "imported"}); err != nil {
		t.Fatalf("reconcileLocalChanges: %v", err)
	}

	if _, ok := eng.state.Entries["imported/a.txt"]; !ok {
		t.Error("imported/a.txt not uploaded")
	}
	if fakeRemoteNames(fs)[".DS_Store"] {
		t.Error(".DS_Store uploaded")
	}
	if _, ok := eng.localTreeFiles["imported/.DS_Store"]; ok {
		t.Error("imported/.DS_Store recorded in the local tree cache")
	}
}

// Files synced before their name was ignored (by an older version) are dropped
// from the index — never trashed remotely or removed locally.
func TestPreviouslySyncedOSMetadataIsForgottenNotDeleted(t *testing.T) {
	eng, fs := newLocalTreeCacheTestEngine(t)
	fs.mu.Lock()
	fs.nodes["f1"] = &fakeNode{storageNode: storageNode{ID: "f1", ParentID: "root", Name: ".DS_Store", NodeType: "file", SizeBytes: 6}, data: []byte("finder")}
	fs.nodes["d1"] = &fakeNode{storageNode: storageNode{ID: "d1", ParentID: "root", Name: ".Trashes", NodeType: "folder"}}
	fs.mu.Unlock()
	writeLocalAll(t, eng, ".DS_Store", "finder")
	if err := os.Mkdir(filepath.Join(eng.folder, ".Trashes"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.state.Entries[".DS_Store"] = SyncEntry{RelPath: ".DS_Store", NodeID: "f1"}
	eng.state.Folders[".Trashes"] = true
	eng.state.FolderIDs[".Trashes"] = "d1"

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll: %v", err)
	}

	names := fakeRemoteNames(fs)
	if !names[".DS_Store"] || !names[".Trashes"] {
		t.Error("previously synced metadata was trashed remotely")
	}
	for _, rel := range []string{".DS_Store", ".Trashes"} {
		if _, err := os.Stat(filepath.Join(eng.folder, rel)); err != nil {
			t.Errorf("previously synced %s was removed locally", rel)
		}
	}
	if _, ok := eng.state.Entries[".DS_Store"]; ok {
		t.Error(".DS_Store still in the sync index")
	}
	if eng.state.Folders[".Trashes"] {
		t.Error(".Trashes still in the sync index")
	}
}

// A remote rename into an ignored name must take the node (and a renamed
// folder's cached contents) out of the cache rather than leave it under its
// old path, and report the files that left so the file pass reacts.
func TestApplyRemoteDeltaDropsNodesRenamedIntoIgnoredNames(t *testing.T) {
	e := newDeltaTestEngine()

	affected := e.applyRemoteDelta([]storageNode{
		{ID: "a1", NodeType: "file", Path: "/docs/.DS_Store"},
	})
	if _, ok := e.remoteTreeFiles["docs/a.txt"]; ok {
		t.Error(`remoteTreeFiles["docs/a.txt"] still present after the rename`)
	}
	if _, ok := e.remoteTreeFiles["docs/.DS_Store"]; ok {
		t.Error(`remoteTreeFiles["docs/.DS_Store"] cached, want it ignored`)
	}
	if len(affected) != 1 || affected[0] != "docs/a.txt" {
		t.Errorf("affected = %v, want [docs/a.txt]", affected)
	}

	affected = e.applyRemoteDelta([]storageNode{
		{ID: "f1", NodeType: "folder", Path: "/.Trashes"},
	})
	if len(e.remoteTreeFiles) != 0 || len(e.remoteTreeFolders) != 0 || len(e.remoteTreeFolderID) != 0 || len(e.remoteTreeIDToRel) != 0 {
		t.Errorf("cache not emptied: files=%v folders=%v ids=%v", e.remoteTreeFiles, e.remoteTreeFolders, e.remoteTreeIDToRel)
	}
	if len(affected) != 1 || affected[0] != "docs/b.txt" {
		t.Errorf("affected = %v, want [docs/b.txt]", affected)
	}
}
