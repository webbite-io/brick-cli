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
