package main

import (
	"os"
	"path/filepath"
	"testing"
)

// newDryRunTestEngine builds a syncEngine with no HTTP client (dryRunClassify
// never makes network calls — it only reads local files and the in-memory
// trees/state passed to it) rooted at a fresh temp folder.
func newDryRunTestEngine(t *testing.T, firstSync bool, conflictMode string) *syncEngine {
	t.Helper()
	return &syncEngine{
		folder:       t.TempDir(),
		firstSync:    firstSync,
		conflictMode: conflictMode,
		state:        &SyncState{Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
	}
}

func writeLocal(t *testing.T, e *syncEngine, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.folder, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunClassifyRemoteOnlyNeverSynced(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be downloaded. Exists remotely but not locally."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyRemoteOnlyButWasSynced(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1", RemoteEtag: "e1", LocalHash: "h1"}
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be deleted remotely. Removed locally."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyLocalOnlyNeverSynced(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	writeLocal(t, e, "a.txt", "hello")
	remote := map[string]storageNode{}
	local := map[string]int64{"a.txt": 5}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be uploaded. Exists locally but not remotely."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyLocalOnlyDeletedOnServerUnchanged(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	writeLocal(t, e, "a.txt", "hello")
	h, err := hashFile(filepath.Join(e.folder, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1", RemoteEtag: "e1", LocalHash: h}
	remote := map[string]storageNode{}
	local := map[string]int64{"a.txt": 5}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be removed locally. Deleted on the server."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyInSync(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	writeLocal(t, e, "a.txt", "hello")
	h, err := hashFile(filepath.Join(e.folder, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1", RemoteEtag: "e1", LocalHash: h}
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{"a.txt": 5}

	if _, ok := dryRunClassify(e, "a.txt", remote, local, nil); ok {
		t.Error("want ok=false: file is already in sync")
	}
}

func TestDryRunClassifySkipsMD5VerifiedFile(t *testing.T) {
	e := newDryRunTestEngine(t, true, "device")
	writeLocal(t, e, "a.txt", "hello")
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{"a.txt": 5}
	verified := map[string]bool{"a.txt": true}

	if _, ok := dryRunClassify(e, "a.txt", remote, local, verified); ok {
		t.Error("want ok=false: content was verified identical via md5, nothing to transfer")
	}
}

func TestDryRunClassifyUnverifiedFirstSyncHonorsConflictMode(t *testing.T) {
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{"a.txt": 5}

	cases := []struct {
		conflictMode string
		want         string
	}{
		{"device", "To be downloaded. Exists both locally and remotely but lacks MD5 checksum."},
		{"brick", "To be uploaded. Exists both locally and remotely but lacks MD5 checksum."},
		{"copy", "To be kept as both copies (local renamed aside). Exists both locally and remotely but lacks MD5 checksum."},
	}
	for _, c := range cases {
		e := newDryRunTestEngine(t, true, c.conflictMode)
		writeLocal(t, e, "a.txt", "hello")

		label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
		if !ok {
			t.Errorf("conflictMode %q: want ok=true", c.conflictMode)
			continue
		}
		if label != c.want {
			t.Errorf("conflictMode %q: label = %q, want %q", c.conflictMode, label, c.want)
		}
	}
}

func TestDryRunClassifyUnverifiedAfterOnboardingFallsBackToRemoteWins(t *testing.T) {
	// firstSync is false here — the gap fixed earlier: a file with no
	// sync-state entry can show up well after onboarding too, and
	// reconcileFile's real behavior for that case (outside firstSync) is a
	// plain remote-wins, not the configured conflict mode. The dry run must
	// report that same behavior, not the onboarding wording.
	e := newDryRunTestEngine(t, false, "brick")
	writeLocal(t, e, "a.txt", "hello")
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{"a.txt": 5}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be downloaded. Exists both locally and remotely but lacks MD5 checksum."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyLocalChangeOnly(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	writeLocal(t, e, "a.txt", "new content")
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1", RemoteEtag: "e1", LocalHash: "stale-hash"}
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{"a.txt": 11}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be uploaded. Local copy changed since last sync."; label != want {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyRemoteChange(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	writeLocal(t, e, "a.txt", "hello")
	h, err := hashFile(filepath.Join(e.folder, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1", RemoteEtag: "stale-etag", LocalHash: h}
	remote := map[string]storageNode{"a.txt": {ID: "n1", Etag: "new-etag"}}
	local := map[string]int64{"a.txt": 5}

	label, ok := dryRunClassify(e, "a.txt", remote, local, nil)
	if !ok {
		t.Fatal("want ok=true")
	}
	if want := "To be downloaded. Remote copy changed since last sync."; want != label {
		t.Errorf("label = %q, want %q", label, want)
	}
}

func TestDryRunClassifyExcludedPathAlwaysSkipped(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	e.excludeDirs = []string{"secret"}
	remote := map[string]storageNode{"secret/a.txt": {ID: "n1", Etag: "e1"}}
	local := map[string]int64{}

	if _, ok := dryRunClassify(e, "secret/a.txt", remote, local, nil); ok {
		t.Error("want ok=false: excluded paths are never reported")
	}
}

func TestDryRunClassifyGoneOnBothSides(t *testing.T) {
	e := newDryRunTestEngine(t, false, "")
	e.state.Entries["a.txt"] = SyncEntry{RelPath: "a.txt", NodeID: "n1"}
	remote := map[string]storageNode{}
	local := map[string]int64{}

	if _, ok := dryRunClassify(e, "a.txt", remote, local, nil); ok {
		t.Error("want ok=false: nothing to report once a file is gone on both sides")
	}
}
