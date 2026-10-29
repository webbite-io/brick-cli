package main

import (
	"mime"
	"testing"
)

func TestContentDispositionNamesTheFile(t *testing.T) {
	cases := []struct {
		name, want string
	}{
		{"report.pdf", `attachment; filename="report.pdf"`},
		{"Ölbild.jpg", `attachment; filename="_lbild.jpg"; filename*=UTF-8''%C3%96lbild.jpg`},
		{`say "hi".txt`, `attachment; filename="say _hi_.txt"; filename*=UTF-8''say%20%22hi%22.txt`},
		{"写真 1.png", `attachment; filename="__ 1.png"; filename*=UTF-8''%E5%86%99%E7%9C%9F%201.png`},
	}
	for _, tc := range cases {
		got := contentDisposition(tc.name)
		if got != tc.want {
			t.Errorf("contentDisposition(%q) = %s, want %s", tc.name, got, tc.want)
		}
		// A standard parser must recover the real name.
		_, params, err := mime.ParseMediaType(got)
		if err != nil {
			t.Errorf("parse %s: %v", got, err)
			continue
		}
		if params["filename"] != tc.name {
			t.Errorf("parsed filename of %q = %q", tc.name, params["filename"])
		}
	}
}
