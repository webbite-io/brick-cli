package main

import (
	"strings"
	"testing"
)

func TestQuotaLineThresholds(t *testing.T) {
	const quota = 1000

	tests := []struct {
		name  string
		used  int64
		color string
	}{
		{"well under the warn threshold", 100, ""},
		{"exactly at the warn threshold", 750, ""},
		{"past the warn threshold", 751, ansiOrange},
		{"exactly at the critical threshold", 950, ansiOrange},
		{"past the critical threshold", 951, ansiRed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &storageQuota{QuotaBytes: quota, UsedBytes: tc.used}
			line := quotaLine(q)
			if line == "" {
				t.Fatal("expected a storage line")
			}
			usage := humanSize(tc.used) + " of " + humanSize(quota) + " used total"
			want := usage
			if tc.color != "" {
				want = tc.color + usage + ansiReset
			}
			if !strings.Contains(line, want) {
				t.Errorf("quotaLine(%d/%d) = %q, want it to contain %q", tc.used, quota, line, want)
			}
			// Only the usage figures are coloured, and only in one colour.
			for _, unwanted := range []string{ansiOrange, ansiRed} {
				if unwanted != tc.color && strings.Contains(line, unwanted) {
					t.Errorf("quotaLine(%d/%d) = %q, unexpected colour %q", tc.used, quota, line, unwanted)
				}
			}
		})
	}
}

// A quota can't be rendered before the first fetch lands, nor for an account
// the server reports no quota for — the banner leaves the line out entirely
// rather than showing a nonsense ratio.
func TestQuotaLineOmitted(t *testing.T) {
	if got := quotaLine(nil); got != "" {
		t.Errorf("quotaLine(nil) = %q, want empty", got)
	}
	if got := quotaLine(&storageQuota{UsedBytes: 42}); got != "" {
		t.Errorf("quotaLine(no quota) = %q, want empty", got)
	}
}

func TestSyncHeaderLines(t *testing.T) {
	withoutQuota := syncHeaderLines(nil)
	if len(withoutQuota) != 2 {
		t.Fatalf("syncHeaderLines(nil) = %d lines, want 2 (commands + separator)", len(withoutQuota))
	}

	q := &storageQuota{QuotaBytes: 536870912000, UsedBytes: 54666913545}
	q.CallingUser.UsedBytes = 30023108123
	withQuota := syncHeaderLines(q)
	if len(withQuota) != 3 {
		t.Fatalf("syncHeaderLines(quota) = %d lines, want 3", len(withQuota))
	}
	if !strings.Contains(withQuota[1], "50.9 GB of 500.0 GB used total (your share 28.0 GB)") {
		t.Errorf("storage line = %q, missing the expected usage figures", withQuota[1])
	}
	// The separator closes the block, and the storage line slots in above it
	// without changing its width.
	if withQuota[2] != withoutQuota[1] {
		t.Errorf("separator changed with a quota present: %q vs %q", withQuota[2], withoutQuota[1])
	}
}

// The live window's cursor-up redraw math counts one screen row per line, so
// truncation has to cut at a visible column and leave escapes uncounted.
func TestTruncateLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		width int
		want  string
	}{
		{"unknown terminal width leaves the line alone", "hello", 0, "hello"},
		{"line shorter than the width", "hello", 10, "hello"},
		{"line exactly at the width", "hello", 5, "hello"},
		{"plain line is cut with an ellipsis", "hello world", 8, "hello w…"},
		{"single column", "hello", 1, "…"},
		{"escapes don't count toward the width", ansiRed + "hello" + ansiReset, 5, ansiRed + "hello" + ansiReset},
		{"cut mid-colour is reset", ansiRed + "hello world" + ansiReset, 8, ansiRed + "hello w…" + ansiReset},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLine(tc.line, tc.width)
			if got != tc.want {
				t.Errorf("truncateLine(%q, %d) = %q, want %q", tc.line, tc.width, got, tc.want)
			}
			if tc.width > 0 && visibleWidth(got) > tc.width {
				t.Errorf("truncateLine(%q, %d) = %q, %d visible columns exceeds the width",
					tc.line, tc.width, got, visibleWidth(got))
			}
		})
	}
}

func TestVisibleWidth(t *testing.T) {
	if got := visibleWidth("plain"); got != 5 {
		t.Errorf("visibleWidth(plain) = %d, want 5", got)
	}
	if got := visibleWidth(ansiPurple + "plain" + ansiReset); got != 5 {
		t.Errorf("visibleWidth(coloured) = %d, want 5", got)
	}
	if got := visibleWidth("a • b"); got != 5 {
		t.Errorf("visibleWidth(multi-byte rune) = %d, want 5", got)
	}
}
