package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAgentReadOnlyRootRejectsWrites(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ro := t.TempDir()
	rw := filepath.Join(ro, "inbox") // a writable root nested inside the read-only one
	if err := os.MkdirAll(rw, 0o755); err != nil {
		t.Fatal(err)
	}
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: ro, readOnly: true}, {path: rw}})

	file := filepath.Join(ro, "notes.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	addFakeFile(fake, "f1", "remote.txt", []byte("remote"))

	rejected := []struct {
		name, method, path string
		body               interface{}
	}{
		{"write", "PUT", "/fs/write?path=" + filepath.Join(ro, "new.txt"), nil},
		{"mkdir", "POST", "/fs/mkdir?path=" + filepath.Join(ro, "dir"), nil},
		{"delete", "DELETE", "/fs/delete?path=" + file, nil},
		{"move out of read-only", "POST", "/fs/move", fromToReq{From: file, To: filepath.Join(rw, "notes.txt")}},
		{"move into read-only", "POST", "/fs/move", fromToReq{From: filepath.Join(rw, "x"), To: filepath.Join(ro, "x")}},
		{"copy into read-only", "POST", "/fs/copy", fromToReq{From: file, To: filepath.Join(ro, "copy.txt")}},
		{"download into read-only", "POST", "/transfer/download", downloadReq{NodeID: "f1", LocalPath: filepath.Join(ro, "remote.txt")}},
	}
	for _, tc := range rejected {
		var errResp struct {
			Error apiError `json:"error"`
		}
		if status := ta.do(tc.method, tc.path, tc.body, &errResp); status != http.StatusForbidden || errResp.Error.Code != "read_only_root" {
			t.Errorf("%s: status %d code %q, want 403 read_only_root", tc.name, status, errResp.Error.Code)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != "hello" {
		t.Errorf("read-only file changed to %q", got)
	}

	// Reading from a read-only root still works, including copying or
	// uploading out of it.
	allowed := []struct {
		name, method, path string
		body               interface{}
	}{
		{"list", "GET", "/fs/list?path=" + ro, nil},
		{"stat", "GET", "/fs/stat?path=" + file, nil},
		{"copy out of read-only", "POST", "/fs/copy", fromToReq{From: file, To: filepath.Join(rw, "copy.txt")}},
		{"upload from read-only", "POST", "/transfer/upload", uploadReq{LocalPath: file, ParentID: "root"}},
		{"download into nested writable root", "POST", "/transfer/download", downloadReq{NodeID: "f1", LocalPath: filepath.Join(rw, "remote.txt")}},
	}
	for _, tc := range allowed {
		if status := ta.do(tc.method, tc.path, tc.body, nil); status != http.StatusOK {
			t.Errorf("%s: status %d, want 200", tc.name, status)
		}
	}

	var roots struct {
		Roots []rootEntry `json:"roots"`
	}
	ta.do("GET", "/fs/roots", nil, &roots)
	if len(roots.Roots) != 2 || !roots.Roots[0].ReadOnly || roots.Roots[1].ReadOnly {
		t.Errorf("GET /fs/roots = %+v, want the first root read-only and the second writable", roots.Roots)
	}
}

func TestAgentRootYAMLForms(t *testing.T) {
	var cfg Config
	src := "agentRoots:\n  - /plain\n  - path: /explicit-ro\n    readOnly: true\n  - path: /writable\n    readOnly: false\n  - path: /mapping-default\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"/plain": true, "/explicit-ro": true, "/writable": false, "/mapping-default": true}
	if len(cfg.AgentRoots) != len(want) {
		t.Fatalf("parsed %d roots, want %d", len(cfg.AgentRoots), len(want))
	}
	for _, r := range cfg.AgentRoots {
		if r.isReadOnly() != want[r.Path] {
			t.Errorf("%s: readOnly %v, want %v", r.Path, r.isReadOnly(), want[r.Path])
		}
	}

	out, err := yaml.Marshal(Config{AgentRoots: cfg.AgentRoots[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "- /plain\n") || strings.Contains(string(out), "path:") {
		t.Errorf("a root with readOnly unset marshalled as %q, want the plain-path form", out)
	}
}

func TestResolveAgentRootsReadOnlyWins(t *testing.T) {
	saved := agentRootsFlag
	t.Cleanup(func() { agentRootsFlag = saved })
	agentRootsFlag = stringList{"/shared"}

	no := false
	roots := resolveAgentRoots(&Config{AgentRoots: []AgentRoot{
		{Path: "/shared", ReadOnly: &no}, // also a flag root, which is always read-only
		{Path: "/writable", ReadOnly: &no},
	}})
	want := []agentRoot{{path: "/shared", readOnly: true}, {path: "/writable", readOnly: false}}
	if !slices.Equal(roots, want) {
		t.Errorf("resolveAgentRoots = %+v, want %+v", roots, want)
	}
}

// stubExcludeDirs is an in-memory excludeDirsSettings.
type stubExcludeDirs struct{ dirs []string }

func (s *stubExcludeDirs) excludeDirs() []string { return s.dirs }
func (s *stubExcludeDirs) setExcludeDirs(dirs []string) error {
	s.dirs = dirs
	return nil
}

func TestAgentExcludeDirsEndpoints(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)

	var errResp struct {
		Error apiError `json:"error"`
	}
	if status := ta.do("GET", "/config/exclude-dirs", nil, &errResp); status != http.StatusServiceUnavailable {
		t.Errorf("GET with no sync running: status %d, want 503", status)
	}

	stub := &stubExcludeDirs{}
	ta.server.settings = stub

	var got struct {
		ExcludeDirs []string `json:"excludeDirs"`
	}
	if status := ta.do("GET", "/config/exclude-dirs", nil, &got); status != http.StatusOK || got.ExcludeDirs == nil || len(got.ExcludeDirs) != 0 {
		t.Errorf("GET with nothing excluded: status %d, excludeDirs %#v, want 200 and []", status, got.ExcludeDirs)
	}

	body := map[string][]string{"excludeDirs": {"/Photos/", "Work//Archive/", "Photos", " Music "}}
	if status := ta.do("PUT", "/config/exclude-dirs", body, &got); status != http.StatusOK {
		t.Fatalf("PUT: status %d, want 200", status)
	}
	want := []string{"Photos", "Work/Archive", "Music"}
	if !slices.Equal(got.ExcludeDirs, want) || !slices.Equal(stub.dirs, want) {
		t.Errorf("PUT stored %v and answered %v, want both %v", stub.dirs, got.ExcludeDirs, want)
	}

	for _, bad := range []interface{}{
		map[string][]string{"excludeDirs": {"../etc"}},
		map[string][]string{"excludeDirs": {"Photos/../.."}},
		map[string][]string{"excludeDirs": {"/"}},
		map[string]string{"something": "else"},
	} {
		if status := ta.do("PUT", "/config/exclude-dirs", bad, nil); status != http.StatusBadRequest {
			t.Errorf("PUT %v: status %d, want 400", bad, status)
		}
	}
	if !slices.Equal(stub.dirs, want) {
		t.Errorf("a rejected PUT changed the list to %v", stub.dirs)
	}
}

func TestSyncExcludeDirsSettingsSavesAndQueues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONFIG_FOLDER", "brick-test")
	cfgPath, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{ClientID: "c1", ActiveAccountID: "acct-1"}
	eng := &syncEngine{}
	notified := 0
	s := &syncExcludeDirsSettings{cfg: cfg, eng: eng, notify: func() { notified++ }}

	if err := s.setExcludeDirs([]string{"Photos"}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Accounts["acct-1"].ExcludeDirs; !slices.Equal(got, []string{"Photos"}) {
		t.Errorf("config.yaml excludeDirs = %v, want [Photos]", got)
	}
	if p := eng.pendingExcludeDirs.Load(); p == nil || !slices.Equal(*p, []string{"Photos"}) {
		t.Errorf("engine was not handed the new list")
	}
	if notified != 1 {
		t.Errorf("notify called %d times, want 1 (to trigger a pass)", notified)
	}
	if got := s.excludeDirs(); !slices.Equal(got, []string{"Photos"}) {
		t.Errorf("excludeDirs() = %v, want [Photos]", got)
	}
}

// Regression test: un-excluding a folder must download it, not trash it.
// While excluded, the folder is recorded in the sync index but never created
// locally, so before reconcileExcludeDirsChange the next pass read it as
// "deleted locally" and soft-deleted it on the server — and a local-only file
// under it as "deleted remotely" and removed it from disk.
func TestUnexcludedFolderIsDownloadedNotTrashed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONFIG_FOLDER", "brick-test")
	syncFolder := t.TempDir()

	fake := newFakeStorageAPI()
	fake.nodes["cam"] = &fakeNode{storageNode: storageNode{ID: "cam", ParentID: "root", Name: "Camera Uploads", NodeType: "folder"}}
	addFakeFile(fake, "photo1", "photo1.jpg", []byte("jpeg"))
	fake.nodes["photo1"].ParentID = "cam"
	server := httptest.NewServer(fake.mux("acct-1"))
	t.Cleanup(server.Close)

	// The second excluded folder, Notes, exists only locally; its file was
	// never uploaded while excluded.
	localOnly := filepath.Join(syncFolder, "Notes", "todo.txt")
	if err := os.MkdirAll(filepath.Dir(localOnly), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localOnly, []byte("milk"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := &syncEngine{
		sc:              &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}},
		folder:          syncFolder,
		accountID:       "acct-1",
		rootID:          "root",
		excludeDirs:     []string{"Camera Uploads", "Notes"},
		firstSync:       true,
		conflictMode:    "device",
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}
	ctx := context.Background()
	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	if !eng.state.Folders["Camera Uploads"] || eng.state.Entries["Notes/todo.txt"].RelPath == "" {
		t.Fatal("setup: expected the excluded folder and file to be recorded in the index")
	}

	eng.firstSync = false
	eng.queueExcludeDirs(nil)
	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	camTrashed := fake.nodes["cam"].IsDeleted
	var uploaded bool
	for _, n := range fake.nodes {
		if n.Name == "todo.txt" && string(n.data) == "milk" {
			uploaded = true
		}
	}
	fake.mu.Unlock()
	if camTrashed {
		t.Error("un-excluded folder was trashed on the server")
	}
	if got, err := os.ReadFile(filepath.Join(syncFolder, "Camera Uploads", "photo1.jpg")); err != nil || string(got) != "jpeg" {
		t.Errorf("un-excluded remote file not downloaded: %q, %v", got, err)
	}
	if _, err := os.Stat(localOnly); err != nil {
		t.Errorf("local-only file under an un-excluded folder was removed: %v", err)
	}
	if !uploaded {
		t.Error("local-only file under an un-excluded folder was not uploaded")
	}
	if len(eng.state.ExcludeDirs) != 0 {
		t.Errorf("state.ExcludeDirs = %v, want it updated to the new (empty) list", eng.state.ExcludeDirs)
	}
}
