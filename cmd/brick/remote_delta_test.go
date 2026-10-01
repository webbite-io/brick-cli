package main

import (
	"context"
	"testing"
)

// newDeltaTestEngine builds a syncEngine with a pre-populated remote-tree
// cache (as if a prior full walk or delta had already seeded it), with no
// HTTP wiring at all -- applyRemoteDelta and rewriteRemotePrefix are pure
// in-memory operations on that cache.
func newDeltaTestEngine() *syncEngine {
	e := &syncEngine{
		remoteTreeFiles: map[string]storageNode{
			"docs/a.txt": {ID: "a1", NodeType: "file", Path: "/docs/a.txt", Etag: "e-a1"},
			"docs/b.txt": {ID: "b1", NodeType: "file", Path: "/docs/b.txt", Etag: "e-b1"},
		},
		remoteTreeFolders: map[string]storageNode{
			"docs": {ID: "f1", NodeType: "folder", Path: "/docs"},
		},
		remoteTreeFolderID: map[string]string{"docs": "f1"},
	}
	e.remoteTreeIDToRel = buildRemoteIDIndex(e.remoteTreeFiles, e.remoteTreeFolders)
	return e
}

// A folder move/rename reported by check-updates carries only the folder's
// own row -- check-updates never bumps a descendant's own updated_at just
// because an ancestor moved. applyRemoteDelta must still relocate every
// cached descendant via rewriteRemotePrefix, not just the folder's own entry.
func TestApplyRemoteDeltaRelocatesFolderDescendantsOnMove(t *testing.T) {
	e := newDeltaTestEngine()

	affected := e.applyRemoteDelta([]storageNode{
		{ID: "f1", NodeType: "folder", Path: "/archive"},
	})

	if _, ok := e.remoteTreeFolders["docs"]; ok {
		t.Error(`remoteTreeFolders["docs"] still present, want it gone after the move`)
	}
	node, ok := e.remoteTreeFolders["archive"]
	if !ok || node.ID != "f1" {
		t.Errorf(`remoteTreeFolders["archive"] = %+v, %v, want the moved folder's node`, node, ok)
	}
	if e.remoteTreeFolderID["archive"] != "f1" {
		t.Errorf(`remoteTreeFolderID["archive"] = %q, want "f1"`, e.remoteTreeFolderID["archive"])
	}

	for oldRel, newRel := range map[string]string{"docs/a.txt": "archive/a.txt", "docs/b.txt": "archive/b.txt"} {
		if _, ok := e.remoteTreeFiles[oldRel]; ok {
			t.Errorf("remoteTreeFiles[%q] still present, want it relocated to %q", oldRel, newRel)
		}
		if _, ok := e.remoteTreeFiles[newRel]; !ok {
			t.Errorf("remoteTreeFiles[%q] missing, want the descendant carried along by the folder's move", newRel)
		}
	}

	if e.remoteTreeIDToRel["f1"] != "archive" {
		t.Errorf(`remoteTreeIDToRel["f1"] = %q, want "archive"`, e.remoteTreeIDToRel["f1"])
	}
	if e.remoteTreeIDToRel["a1"] != "archive/a.txt" {
		t.Errorf(`remoteTreeIDToRel["a1"] = %q, want "archive/a.txt"`, e.remoteTreeIDToRel["a1"])
	}

	// The folder itself is the only file pass entry that isn't in scope
	// (folders aren't reconcileFile candidates); the delta carried no file
	// nodes this round.
	if len(affected) != 0 {
		t.Errorf("affected = %v, want empty (only a folder node was in this delta)", affected)
	}
}

// A trashed node must be removed from the cache using the rel path it was
// last known under, not the (possibly stale, possibly empty) path on the
// IsDeleted node itself, and reported as affected so the file pass gets a
// chance to drop any local copy or stale index entry.
func TestApplyRemoteDeltaRemovesTrashedFile(t *testing.T) {
	e := newDeltaTestEngine()

	affected := e.applyRemoteDelta([]storageNode{
		{ID: "b1", NodeType: "file", Path: "/docs/b.txt", IsDeleted: true},
	})

	if _, ok := e.remoteTreeFiles["docs/b.txt"]; ok {
		t.Error(`remoteTreeFiles["docs/b.txt"] still present, want it removed`)
	}
	if _, ok := e.remoteTreeIDToRel["b1"]; ok {
		t.Error(`remoteTreeIDToRel["b1"] still present, want it cleared`)
	}
	if len(affected) != 1 || affected[0] != "docs/b.txt" {
		t.Errorf("affected = %v, want [\"docs/b.txt\"]", affected)
	}
	// The unrelated file must survive untouched.
	if _, ok := e.remoteTreeFiles["docs/a.txt"]; !ok {
		t.Error(`remoteTreeFiles["docs/a.txt"] removed, want it untouched`)
	}
}

// A file moved independently of its folder (same ID, different path) must be
// relocated in the cache, and both its old and new rel reported as affected
// -- the old one so a stale index entry left behind (if pass 0's
// applyRemoteFileMoves didn't mirror the move locally, e.g. no local copy to
// move) still gets cleaned up.
func TestApplyRemoteDeltaRelocatesMovedFile(t *testing.T) {
	e := newDeltaTestEngine()

	affected := e.applyRemoteDelta([]storageNode{
		{ID: "a1", NodeType: "file", Path: "/docs/renamed.txt", Etag: "e-a1"},
	})

	if _, ok := e.remoteTreeFiles["docs/a.txt"]; ok {
		t.Error(`remoteTreeFiles["docs/a.txt"] still present, want it relocated`)
	}
	node, ok := e.remoteTreeFiles["docs/renamed.txt"]
	if !ok || node.ID != "a1" {
		t.Errorf(`remoteTreeFiles["docs/renamed.txt"] = %+v, %v, want the moved node`, node, ok)
	}
	if e.remoteTreeIDToRel["a1"] != "docs/renamed.txt" {
		t.Errorf(`remoteTreeIDToRel["a1"] = %q, want "docs/renamed.txt"`, e.remoteTreeIDToRel["a1"])
	}

	want := map[string]bool{"docs/a.txt": true, "docs/renamed.txt": true}
	got := map[string]bool{}
	for _, rel := range affected {
		got[rel] = true
	}
	if len(got) != len(want) {
		t.Fatalf("affected = %v, want both the old and new rel", affected)
	}
	for rel := range want {
		if !got[rel] {
			t.Errorf("affected missing %q", rel)
		}
	}
}

// A folder this client created itself, then moved remotely before check-updates
// ever reported its creation. The move's row carries only the new path, so the
// ID index ensureRemoteFolder maintains is the only thing that can say which rel
// the folder moved away from. Without it the old rel survives in the cache as a
// folder that no longer exists — recreated on disk by pass 2, and then claimed
// by the same node ID as the new rel, leaving applyRemoteFolderMoves to choose
// between the two by map-iteration order and rename the folder back and forth
// until the next full walk.
func TestEnsureRemoteFolderIndexesCreatedFolderForLaterMoves(t *testing.T) {
	sc, _ := newTestTransferClient(t)
	e := &syncEngine{
		sc:                 sc,
		rootID:             "root",
		remoteTreeFiles:    map[string]storageNode{},
		remoteTreeFolders:  map[string]storageNode{},
		remoteTreeFolderID: map[string]string{"": "root"},
		state: &SyncState{
			Entries:   map[string]SyncEntry{},
			Folders:   map[string]bool{},
			FolderIDs: map[string]string{},
		},
	}
	e.remoteTreeIDToRel = buildRemoteIDIndex(e.remoteTreeFiles, e.remoteTreeFolders)

	id, err := e.ensureRemoteFolder(context.Background(), "Docs", e.remoteTreeFolders, e.remoteTreeFolderID)
	if err != nil {
		t.Fatalf("ensureRemoteFolder: %v", err)
	}
	if e.remoteTreeIDToRel[id] != "Docs" {
		t.Fatalf("remoteTreeIDToRel[%q] = %q, want %q right after creating it", id, e.remoteTreeIDToRel[id], "Docs")
	}

	e.applyRemoteDelta([]storageNode{{ID: id, NodeType: "folder", Path: "/Archive"}})

	if _, ok := e.remoteTreeFolders["Docs"]; ok {
		t.Error(`remoteTreeFolders["Docs"] still cached after the move: a phantom folder`)
	}
	if _, ok := e.remoteTreeFolderID["Docs"]; ok {
		t.Error(`remoteTreeFolderID["Docs"] still cached after the move`)
	}
	if node, ok := e.remoteTreeFolders["Archive"]; !ok || node.ID != id {
		t.Errorf(`remoteTreeFolders["Archive"] = %+v (ok=%v), want the moved folder`, node, ok)
	}
	if e.remoteTreeIDToRel[id] != "Archive" {
		t.Errorf(`remoteTreeIDToRel[%q] = %q, want "Archive"`, id, e.remoteTreeIDToRel[id])
	}
}
