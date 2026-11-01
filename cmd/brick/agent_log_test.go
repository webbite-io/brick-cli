package main

import (
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every endpoint registered in agentServer.handler has a name for the status
// window, so none falls back to method and path.
func TestRemoteControlActionsCoverEveryEndpoint(t *testing.T) {
	b, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	re := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ [^"]+)"`)
	matches := re.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("found no registered endpoints")
	}
	for _, m := range matches {
		if _, ok := remoteControlActions[m[1]]; !ok {
			t.Errorf("no remoteControlActions entry for %q", m[1])
		}
	}
}

func TestRemoteControlLogLine(t *testing.T) {
	for _, tc := range []struct {
		method, path, target string
		status               int
		want                 string
	}{
		{"GET", "/fs/roots", "", 200, "Remote Control: Listing roots."},
		{"GET", "/fs/read", "a.txt", 200, `Remote Control: Reading "a.txt".`},
		{"GET", "/fs/read", "", 200, "Remote Control: Reading a file."},
		{"POST", "/fs/mkdir", "Photos", 200, `Remote Control: Creating the folder "Photos".`},
		{"GET", "/transfers/abc123", "", 200, "Remote Control: Checking a transfer."},
		{"DELETE", "/transfers/abc123", "", 204, "Remote Control: Cancelling a transfer."},
		{"GET", "/fs/read", "a.txt", 404, `Remote Control: Reading "a.txt" failed (404).`},
		{"GET", "/nope", "", 404, "Remote Control: GET /nope 404 5ms"},
	} {
		if got := remoteControlLogLine(tc.method, tc.path, tc.target, tc.status, 5*time.Millisecond); got != tc.want {
			t.Errorf("%s %s %d: got %q, want %q", tc.method, tc.path, tc.status, got, tc.want)
		}
	}
}

// remoteControlTarget finds the file name in the path query parameter or the
// JSON body, without its folders, and leaves the body intact for forwarding.
func TestRemoteControlTarget(t *testing.T) {
	for _, tc := range []struct {
		method, target, body, want string
	}{
		{"GET", "/fs/read?path=" + url.QueryEscape("/home/me/docs/a.txt"), "", "a.txt"},
		{"GET", "/fs/list?path=" + url.QueryEscape(`C:\Users\me\Photos\`), "", "Photos"},
		{"POST", "/fs/move", `{"from":"/home/me/old.txt","to":"/home/me/new.txt"}`, "old.txt"},
		{"POST", "/transfer/upload", `{"localPath":"/home/me/b.pdf","parentId":"x"}`, "b.pdf"},
		{"POST", "/transfer/download", `not json`, ""},
		{"POST", "/fs/delete-batch", `{"paths":["/home/me/a.txt"]}`, ""},
		{"GET", "/fs/roots", "", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		if got := remoteControlTarget(req); got != tc.want {
			t.Errorf("%s %s: got %q, want %q", tc.method, tc.target, got, tc.want)
		}
		rest, _ := io.ReadAll(req.Body)
		if string(rest) != tc.body {
			t.Errorf("%s %s: body left as %q, want %q", tc.method, tc.target, rest, tc.body)
		}
	}
}
