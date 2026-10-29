package main

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestAgentListSortFilterAndPaginate(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)

	// name, size, modification time: each order is distinct from the others.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	files := []struct {
		name string
		size int
		age  time.Duration
	}{
		{"b.txt", 30, 1 * time.Hour},
		{"a.txt", 10, 3 * time.Hour},
		{"c.txt", 20, 2 * time.Hour},
		{".hidden", 5, 4 * time.Hour},
	}
	for _, f := range files {
		p := filepath.Join(ta.root, f.name)
		if err := os.WriteFile(p, make([]byte, f.size), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := base.Add(-f.age)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	list := func(query string) []string {
		t.Helper()
		var out struct {
			Entries []fsEntry `json:"entries"`
		}
		if status := ta.do("GET", "/fs/list?path="+ta.root+query, nil, &out); status != http.StatusOK {
			t.Fatalf("GET /fs/list%s: status %d, want 200", query, status)
		}
		names := make([]string, len(out.Entries))
		for i, e := range out.Entries {
			names[i] = e.Name
		}
		return names
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{".hidden", "a.txt", "b.txt", "c.txt"}}, // default: name asc, hidden shown
		{"&sortOrder=desc", []string{"c.txt", "b.txt", "a.txt", ".hidden"}},
		{"&sort=sizeBytes", []string{".hidden", "a.txt", "c.txt", "b.txt"}},
		{"&sort=sizeBytes&sortOrder=desc", []string{"b.txt", "c.txt", "a.txt", ".hidden"}},
		{"&sort=updatedAt", []string{".hidden", "a.txt", "c.txt", "b.txt"}},
		{"&sort=updatedAt&sortOrder=desc&showHidden=false", []string{"b.txt", "c.txt", "a.txt"}},
		{"&showHidden=true", []string{".hidden", "a.txt", "b.txt", "c.txt"}},
		{"&showHidden=false", []string{"a.txt", "b.txt", "c.txt"}},
		{"&limit=2", []string{".hidden", "a.txt"}},
		{"&limit=2&offset=2", []string{"b.txt", "c.txt"}},
		{"&limit=0&offset=1", []string{"a.txt", "b.txt", "c.txt"}},
		{"&offset=9", []string{}},
		{"&sort=sizeBytes&sortOrder=desc&showHidden=false&limit=1&offset=1", []string{"c.txt"}},
	}
	for _, tc := range cases {
		if got := list(tc.query); !slices.Equal(got, tc.want) {
			t.Errorf("GET /fs/list%s = %v, want %v", tc.query, got, tc.want)
		}
	}

	// count is the total after filtering, before paging.
	var out struct {
		Entries []fsEntry `json:"entries"`
		Count   int       `json:"count"`
	}
	if status := ta.do("GET", "/fs/list?path="+ta.root+"&showHidden=false&limit=1", nil, &out); status != http.StatusOK || out.Count != 3 || len(out.Entries) != 1 {
		t.Errorf("GET /fs/list&showHidden=false&limit=1: status %d, count %d, %d entries; want 200, 3, 1", status, out.Count, len(out.Entries))
	}

	for _, bad := range []string{"&sort=createdAt", "&sortOrder=up", "&showHidden=maybe", "&limit=-1", "&offset=x"} {
		if status := ta.do("GET", "/fs/list?path="+ta.root+bad, nil, nil); status != http.StatusBadRequest {
			t.Errorf("GET /fs/list%s: status %d, want 400", bad, status)
		}
	}
}
